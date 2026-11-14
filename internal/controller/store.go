package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/settle"
)

// Kinds the store tracks.
const (
	KindItem         = "Item"
	KindChange       = "Change"
	KindTeam         = "Team"
	KindPerson       = "Person"
	KindInstallation = "Installation"
	KindBudget       = "Budget"
)

type objKey struct{ kind, name string }

// store reads objects through the informer cache and writes them to the API server. It remembers the
// objects this replica wrote until the informers deliver them, so that:
//   - a reconcile never decides from a cached copy older than its own last write (the overlay), and
//   - the settle tracker sees every write not yet seen by the informers (contract §3.3).
//
// Only the leader writes engine objects, so the overlay is exact.
type store struct {
	c       client.Client
	ns      string
	tracker *settle.Tracker
	writes  atomic.Int64

	mu      sync.Mutex
	written map[objKey]client.Object
	deleted map[objKey]bool
	seen    map[objKey]uint64
	gone    map[objKey]bool
}

func newStore(c client.Client, ns string, t *settle.Tracker) *store {
	s := &store{c: c, ns: ns, tracker: t, written: map[objKey]client.Object{}, deleted: map[objKey]bool{},
		seen: map[objKey]uint64{}, gone: map[objKey]bool{}}
	return s
}

func (s *store) start() { s.tracker.AddProbe("writes", s.idle) }

func (s *store) stop() {
	s.tracker.RemoveProbe("writes")
	s.mu.Lock()
	s.written, s.deleted = map[objKey]client.Object{}, map[objKey]bool{}
	s.mu.Unlock()
	s.tracker.Poke()
}

func (s *store) idle() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.written) == 0 && len(s.deleted) == 0 {
		return true, ""
	}
	var names []string
	for k := range s.written {
		names = append(names, k.kind+"/"+k.name)
	}
	for k := range s.deleted {
		names = append(names, "deleted "+k.kind+"/"+k.name)
	}
	sort.Strings(names)
	return false, fmt.Sprintf("writes not yet seen by the informers: %v", names)
}

func rvOf(o client.Object) uint64 {
	n, _ := strconv.ParseUint(o.GetResourceVersion(), 10, 64)
	return n
}

// handler returns the informer event handler that marks this kind's writes as seen.
func (s *store) handler(kind string) toolscache.ResourceEventHandler {
	seen := func(obj any) {
		o, ok := obj.(client.Object)
		if !ok {
			return
		}
		k := objKey{kind, o.GetName()}
		rv := rvOf(o)
		s.mu.Lock()
		if rv > s.seen[k] {
			s.seen[k] = rv
		}
		delete(s.gone, k)
		if w, ok := s.written[k]; ok && rvOf(w) <= rv {
			delete(s.written, k)
		}
		s.mu.Unlock()
		s.tracker.Poke()
	}
	return toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { seen(obj) },
		UpdateFunc: func(_, obj any) { seen(obj) },
		DeleteFunc: func(obj any) {
			if d, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			o, ok := obj.(client.Object)
			if !ok {
				return
			}
			k := objKey{kind, o.GetName()}
			s.mu.Lock()
			delete(s.written, k)
			delete(s.deleted, k)
			delete(s.seen, k)
			s.gone[k] = true
			s.mu.Unlock()
			s.tracker.Poke()
		},
	}
}

func (s *store) record(kind string, obj client.Object) {
	k := objKey{kind, obj.GetName()}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.deleted, k)
	delete(s.gone, k)
	if rvOf(obj) <= s.seen[k] {
		return
	}
	s.written[k] = obj.DeepCopyObject().(client.Object)
}

// get reads name into obj: the newer of the cached copy and the last written one.
func (s *store) get(ctx context.Context, kind, name string, obj client.Object) error {
	k := objKey{kind, name}
	s.mu.Lock()
	if s.deleted[k] {
		s.mu.Unlock()
		return apierrors.NewNotFound(schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: kind}, name)
	}
	w := s.written[k]
	s.mu.Unlock()
	err := s.c.Get(ctx, client.ObjectKey{Namespace: s.ns, Name: name}, obj)
	if w != nil && (apierrors.IsNotFound(err) || (err == nil && rvOf(w) > rvOf(obj))) {
		copyInto(obj, w)
		return nil
	}
	return err
}

func copyInto(dst, src client.Object) {
	reflect.ValueOf(dst).Elem().Set(reflect.ValueOf(src.DeepCopyObject()).Elem())
}

// overlay corrects a cached list by the writes not yet seen: newer written copies replace cached
// ones, deleted objects go, created ones are added when match accepts them (nil accepts all).
func overlay[T any, PT interface {
	*T
	client.Object
}](s *store, kind string, cached []T, match func(PT) bool) []PT {
	s.mu.Lock()
	written := map[string]client.Object{}
	deleted := map[string]bool{}
	for k, w := range s.written {
		if k.kind == kind {
			written[k.name] = w
		}
	}
	for k := range s.deleted {
		if k.kind == kind {
			deleted[k.name] = true
		}
	}
	s.mu.Unlock()

	var out []PT
	have := map[string]bool{}
	for i := range cached {
		o := PT(&cached[i])
		name := o.GetName()
		have[name] = true
		if deleted[name] {
			continue
		}
		if w, ok := written[name]; ok && rvOf(w) > rvOf(o) {
			o = w.DeepCopyObject().(PT)
		}
		if match == nil || match(o) {
			out = append(out, o)
		}
	}
	for name, w := range written {
		if have[name] || deleted[name] {
			continue
		}
		o := w.DeepCopyObject().(PT)
		if match == nil || match(o) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out
}

func (s *store) items(ctx context.Context, match func(*v1alpha1.Item) bool, opts ...client.ListOption) ([]*v1alpha1.Item, error) {
	var l v1alpha1.ItemList
	if err := s.c.List(ctx, &l, append(opts, client.InNamespace(s.ns))...); err != nil {
		return nil, err
	}
	return overlay(s, KindItem, l.Items, match), nil
}

func (s *store) changes(ctx context.Context, match func(*v1alpha1.Change) bool, opts ...client.ListOption) ([]*v1alpha1.Change, error) {
	var l v1alpha1.ChangeList
	if err := s.c.List(ctx, &l, append(opts, client.InNamespace(s.ns))...); err != nil {
		return nil, err
	}
	return overlay(s, KindChange, l.Items, match), nil
}

func (s *store) teams(ctx context.Context) ([]*v1alpha1.Team, error) {
	var l v1alpha1.TeamList
	if err := s.c.List(ctx, &l, client.InNamespace(s.ns)); err != nil {
		return nil, err
	}
	return overlay[v1alpha1.Team](s, KindTeam, l.Items, nil), nil
}

func (s *store) people(ctx context.Context) ([]*v1alpha1.Person, error) {
	var l v1alpha1.PersonList
	if err := s.c.List(ctx, &l, client.InNamespace(s.ns)); err != nil {
		return nil, err
	}
	return overlay[v1alpha1.Person](s, KindPerson, l.Items, nil), nil
}

func (s *store) installations(ctx context.Context) ([]*v1alpha1.Installation, error) {
	var l v1alpha1.InstallationList
	if err := s.c.List(ctx, &l, client.InNamespace(s.ns)); err != nil {
		return nil, err
	}
	return overlay[v1alpha1.Installation](s, KindInstallation, l.Items, nil), nil
}

// create creates obj in the namespace.
func (s *store) create(ctx context.Context, kind string, obj client.Object) error {
	obj.SetNamespace(s.ns)
	if err := s.c.Create(ctx, obj); err != nil {
		return err
	}
	s.writes.Add(1)
	s.record(kind, obj)
	return nil
}

// patchStatus writes the difference between base and obj's status as a merge patch (no
// resourceVersion precondition: the Item reconciler and the team pass write disjoint fields).
func (s *store) patchStatus(ctx context.Context, kind string, obj, base client.Object) error {
	patch := client.MergeFrom(base)
	if full, ok := fullStatusPatch(obj, base); ok {
		// The first status write sends all of it: the schema requires fields a diff from a zero status
		// would leave out.
		patch = full
	}
	if err := s.c.Status().Patch(ctx, obj, patch); err != nil {
		return err
	}
	s.writes.Add(1)
	s.record(kind, obj)
	return nil
}

// delete deletes obj.
func (s *store) delete(ctx context.Context, kind string, obj client.Object) error {
	k := objKey{kind, obj.GetName()}
	if err := s.c.Delete(ctx, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	s.writes.Add(1)
	s.mu.Lock()
	delete(s.written, k)
	if !s.gone[k] {
		s.deleted[k] = true
	}
	s.mu.Unlock()
	return nil
}

// fullStatusPatch is a merge patch with obj's whole status, when base's status is still the zero value.
func fullStatusPatch(obj, base client.Object) (client.Patch, bool) {
	statusOf := func(o any) (json.RawMessage, error) {
		b, err := json.Marshal(o)
		if err != nil {
			return nil, err
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, err
		}
		return m["status"], nil
	}
	zero := reflect.New(reflect.TypeOf(base).Elem()).Interface()
	zs, err1 := statusOf(zero)
	bs, err2 := statusOf(base)
	if err1 != nil || err2 != nil || string(zs) != string(bs) {
		return nil, false
	}
	os, err := statusOf(obj)
	if err != nil {
		return nil, false
	}
	data, err := json.Marshal(map[string]json.RawMessage{"status": os})
	if err != nil {
		return nil, false
	}
	return client.RawPatch(types.MergePatchType, data), true
}
