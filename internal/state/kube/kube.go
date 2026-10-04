// Package kube is the Kubernetes store of `beekeeper serve`: the shared state
// of the Giant Swarm installations as custom resources of
// beekeeper.giantswarm.io instead of one document under a file lock.
//
// Each record kind of the state document is a kind of its own: the grants
// and the release time of an installation are its Environment, a lane's
// merges its MergeLane, a hold a Hold, a note a Note in the filer's team
// namespace, an agent a RosterEntry in its team's namespace. Concurrency is
// per object: an update writes the one object it changes under the
// resourceVersion it read, so updates of different installations never
// contend, and a lost race reruns the update on what won it.
//
// The rest of the document (the supervisor, timers, records, the merge
// gate's processes, side files) belongs to the machine and has no home here:
// an update that changes it is refused, never kept somewhere else.
package kube

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/beekeeper/internal/feed"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

// Store is the state.Store over the beekeeper.giantswarm.io resources.
//
// Update may run its function more than once: when another writer changed
// the object in between, it rereads and reruns. The function must change
// nothing but the state it is given.
type Store struct {
	c client.Client
	// timeout bounds each call against the API server.
	timeout time.Duration
	// attempts bounds the reruns of one Update after lost races.
	attempts int

	// idMu guards lastID, the id of the last Event recorded or found: ids
	// only grow, across restarts too (the first record reads the Events'
	// highest).
	idMu    sync.Mutex
	lastID  int64
	idsRead bool
}

var _ state.Store = (*Store)(nil)

// ErrLocal is the refusal of what belongs to the machine, not to the shared
// state: side files, events of no object.
var ErrLocal = errors.New("the Kubernetes store keeps the shared state only; this stays on the machine")

// HeldError is a claim's refusal: the Environment is held by someone else.
type HeldError struct {
	Environment string
	Holder      v1alpha1.Holder
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("%s is held by %s since %s: %s", e.Environment, e.Holder.Party.Name,
		e.Holder.Since.UTC().Format(time.RFC3339), e.Holder.Purpose)
}

// Scheme returns a scheme with the core kinds (Events, Namespaces) and
// beekeeper's.
func Scheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// New returns the store over c, whose scheme knows beekeeper's kinds.
func New(c client.Client) *Store {
	return &Store{c: c, timeout: 30 * time.Second, attempts: 5}
}

// NewForConfig returns the store over the API server cfg names.
func NewForConfig(cfg *rest.Config) (*Store, error) {
	s, err := Scheme()
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		return nil, err
	}
	return New(c), nil
}

func (s *Store) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.timeout)
}

// Dir is empty: the store has no directory; side files stay on the machine.
func (s *Store) Dir() string { return "" }

// Read returns the shared state.
func (s *Store) Read() (*state.State, error) {
	ctx, cancel := s.ctx()
	defer cancel()
	snap, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	return snap.state(), nil
}

// Peek is Read: no writer is waited on.
func (s *Store) Peek() (*state.State, error) { return s.Read() }

// Update runs fn on the shared state and writes the one object it changed,
// under the resourceVersion read, with fn's events as Kubernetes Events on
// it. A lost race reruns fn on the winner's state. An update that changes
// more than one object, or anything without a home in the resources, is
// refused with nothing written.
func (s *Store) Update(fn func(*state.State) ([]state.Event, error)) error {
	ctx, cancel := s.ctx()
	defer cancel()
	for range s.attempts {
		snap, err := s.load(ctx)
		if err != nil {
			return err
		}
		st := snap.state()
		events, err := fn(st)
		if err != nil {
			return err
		}
		want, err := snap.apply(st)
		if err != nil {
			return err
		}
		if err := homeless(st, want.state()); err != nil {
			return err
		}
		changes := snap.diff(want)
		switch {
		case len(changes) == 0:
			if len(events) > 0 {
				return fmt.Errorf("%d events without a changed object: %w", len(events), ErrLocal)
			}
			return nil
		case len(changes) > 1:
			names := make([]string, len(changes))
			for i, c := range changes {
				names[i] = c.ref()
			}
			return fmt.Errorf("the update changes %s; the Kubernetes store writes one object per update", strings.Join(names, ", "))
		}
		obj, err := s.write(ctx, changes[0])
		if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
			continue
		}
		if err != nil {
			return err
		}
		return s.record(ctx, obj, events)
	}
	return fmt.Errorf("the update lost %d races in a row", s.attempts)
}

// Log refuses: an event of no object stays in the machine's log.
func (s *Store) Log(events ...state.Event) error {
	return fmt.Errorf("%d events: %w", len(events), ErrLocal)
}

// ReadFile refuses: side files stay on the machine.
func (s *Store) ReadFile(name string, _ any) (bool, error) {
	return false, fmt.Errorf("side file %s: %w", name, ErrLocal)
}

// WriteFile refuses: side files stay on the machine.
func (s *Store) WriteFile(name string, _ any) error {
	return fmt.Errorf("side file %s: %w", name, ErrLocal)
}

// Holder returns who holds the Environment, nil while it is free.
func (s *Store) Holder(name string) (*v1alpha1.Holder, error) {
	ctx, cancel := s.ctx()
	defer cancel()
	env := &v1alpha1.Environment{}
	if err := s.c.Get(ctx, client.ObjectKey{Name: name}, env); err != nil {
		return nil, environmentErr(name, err)
	}
	return env.Status.Holder, nil
}

// Claim makes h the holder of the Environment. Of two concurrent claims one
// wins; the other gets a HeldError naming the winner. A claim by the holder
// itself renews nothing and succeeds.
func (s *Store) Claim(name string, h v1alpha1.Holder) error {
	return s.holder(name, func(env *v1alpha1.Environment) (bool, error) {
		if cur := env.Status.Holder; cur != nil {
			if party(cur.Party).Is(party(h.Party)) {
				return false, nil
			}
			return false, &HeldError{Environment: name, Holder: *cur}
		}
		env.Status.Holder = &h
		return true, nil
	}, state.Event{At: h.Since.Time, By: party(h.Party), Verb: "lease.claim", Detail: name + ": " + h.Purpose})
}

// Release frees the Environment held by p and records the release time; a
// release by anyone else is refused with the holder named.
func (s *Store) Release(name string, p state.Party, now time.Time) error {
	return s.Free(name, p, p, now)
}

// Free frees the Environment while holder holds it, recorded as by's: the
// holder's own release, or another's (the owning team's supervisor) as a
// forced one. Another holder than holder is refused with the holder named.
func (s *Store) Free(name string, holder, by state.Party, now time.Time) error {
	ev := state.Event{At: now, By: by, Verb: "lease.release", Detail: name}
	if own := holder.Is(by) || (holder.Person != "" && holder.Person == by.Person); !own {
		ev.Verb, ev.Detail = "lease.force-release", name+" held by "+holder.Name
	}
	return s.holder(name, func(env *v1alpha1.Environment) (bool, error) {
		cur := env.Status.Holder
		if cur == nil {
			return false, nil
		}
		if !party(cur.Party).Is(holder) {
			return false, &HeldError{Environment: name, Holder: *cur}
		}
		env.Status.Holder = nil
		env.Status.Released = &metav1.Time{Time: now}
		return true, nil
	}, ev)
}

// Environments returns every Environment, by name.
func (s *Store) Environments() ([]v1alpha1.Environment, error) {
	ctx, cancel := s.ctx()
	defer cancel()
	l := &v1alpha1.EnvironmentList{}
	if err := s.c.List(ctx, l); err != nil {
		return nil, err
	}
	slices.SortFunc(l.Items, func(a, b v1alpha1.Environment) int { return strings.Compare(a.Name, b.Name) })
	return l.Items, nil
}

// Lanes returns every MergeLane, by name.
func (s *Store) Lanes() ([]v1alpha1.MergeLane, error) {
	ctx, cancel := s.ctx()
	defer cancel()
	l := &v1alpha1.MergeLaneList{}
	if err := s.c.List(ctx, l); err != nil {
		return nil, err
	}
	slices.SortFunc(l.Items, func(a, b v1alpha1.MergeLane) int { return strings.Compare(a.Name, b.Name) })
	return l.Items, nil
}

// Audit records e as a Kubernetes Event on obj (its kind, namespace and
// name; the rest is read), or, without obj or once obj is gone, on the
// namespace of team: a call that changed nothing still leaves its Event.
func (s *Store) Audit(obj client.Object, team string, e state.Event) error {
	ctx, cancel := s.ctx()
	defer cancel()
	if obj != nil {
		err := s.c.Get(ctx, client.ObjectKeyFromObject(obj), obj)
		if err == nil {
			return s.record(ctx, obj, []state.Event{e})
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
	}
	ns := &corev1.Namespace{}
	if err := s.c.Get(ctx, client.ObjectKey{Name: TeamNamespace(team)}, ns); err != nil {
		return fmt.Errorf("auditing %s: the team namespace: %w", e.Verb, err)
	}
	return s.record(ctx, ns, []state.Event{e})
}

// HoldObject is the Hold of target. It and the other *Object functions name
// a record of the state document for Audit.
func HoldObject(target string) client.Object {
	return &v1alpha1.Hold{ObjectMeta: metav1.ObjectMeta{Name: holdName(target)}}
}

// NoteObject is note id of team's namespace.
func NoteObject(team string, id int) client.Object {
	return &v1alpha1.Note{ObjectMeta: metav1.ObjectMeta{Namespace: teamNamespace(team), Name: "note-" + strconv.Itoa(id)}}
}

// RosterObject is p's roster entry.
func RosterObject(p state.Party) client.Object {
	return &v1alpha1.RosterEntry{ObjectMeta: metav1.ObjectMeta{Namespace: teamNamespace(p.Team), Name: rosterName(p)}}
}

// EnvironmentObject is the Environment name.
func EnvironmentObject(name string) client.Object {
	return &v1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// LaneObject is the MergeLane name.
func LaneObject(name string) client.Object {
	return &v1alpha1.MergeLane{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// TeamNamespace is a team's namespace, beekeeper-<team>.
func TeamNamespace(team string) string { return teamNamespace(team) }

// holder changes an Environment's holder by compare-and-swap: a conflict
// rereads, so the loser of a race judges the winner's holder.
func (s *Store) holder(name string, change func(*v1alpha1.Environment) (bool, error), event state.Event) error {
	ctx, cancel := s.ctx()
	defer cancel()
	for range s.attempts {
		env := &v1alpha1.Environment{}
		if err := s.c.Get(ctx, client.ObjectKey{Name: name}, env); err != nil {
			return environmentErr(name, err)
		}
		changed, err := change(env)
		if err != nil || !changed {
			return err
		}
		env.Status.ObservedGeneration = env.Generation
		err = s.c.Status().Update(ctx, env)
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return err
		}
		return s.record(ctx, env, []state.Event{event})
	}
	return fmt.Errorf("%s: the claim lost %d races in a row", name, s.attempts)
}

func environmentErr(name string, err error) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%s is not an Environment on this installation", name)
	}
	return err
}

// Events returns the last n Kubernetes Events beekeeper recorded that keep
// accepts, oldest first.
func (s *Store) Events(n int, keep func(state.Event) bool) ([]state.Event, error) {
	ctx, cancel := s.ctx()
	defer cancel()
	list := &corev1.EventList{}
	if err := s.c.List(ctx, list, client.MatchingLabels{managedBy: beekeeper}); err != nil {
		return nil, err
	}
	var out []state.Event
	for _, ev := range list.Items {
		e := state.Event{At: ev.EventTime.Time, Verb: ev.Reason, Detail: ev.Message}
		if raw := ev.Annotations[byAnnotation]; raw != "" {
			var p v1alpha1.Party
			if err := json.Unmarshal([]byte(raw), &p); err == nil {
				e.By = party(p)
			}
		}
		if keep == nil || keep(e) {
			out = append(out, e)
		}
	}
	slices.SortStableFunc(out, func(a, b state.Event) int { return a.At.Compare(b.At) })
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

// FeedEvent is a beekeeper Event in the feed's schema.
func FeedEvent(ev *corev1.Event) (feed.Event, error) {
	var by v1alpha1.Party
	if raw := ev.Annotations[byAnnotation]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &by); err != nil {
			return feed.Event{}, fmt.Errorf("event %s/%s: %s: %w", ev.Namespace, ev.Name, byAnnotation, err)
		}
	}
	actor := feed.Party{Name: by.Name, Person: by.Person, Team: by.Team, Host: by.Host}
	return feed.Event{
		ID: eventID(ev), Kind: ev.Reason, Subject: ev.InvolvedObject.Kind + "/" + ev.InvolvedObject.Name,
		Actor: actor, Time: ev.EventTime.UTC(), Line: feed.Line(ev.Reason, actor, ev.Message),
	}, nil
}

// Feed returns the last n events of the shared state in the feed's schema,
// oldest first: the changes, not the audit of the calls that changed
// nothing (serve.*).
func (s *Store) Feed(n int) ([]feed.Event, error) {
	ctx, cancel := s.ctx()
	defer cancel()
	list := &corev1.EventList{}
	if err := s.c.List(ctx, list, client.MatchingLabels{managedBy: beekeeper}); err != nil {
		return nil, err
	}
	out := []feed.Event{}
	for i := range list.Items {
		ev := &list.Items[i]
		if strings.HasPrefix(ev.Reason, "serve.") {
			continue
		}
		fe, err := FeedEvent(ev)
		if err != nil {
			return nil, err
		}
		out = append(out, fe)
	}
	slices.SortFunc(out, func(a, b feed.Event) int { return strings.Compare(a.ID, b.ID) })
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

// EventLabels are the labels of every Event beekeeper records.
func EventLabels() map[string]string { return map[string]string{managedBy: beekeeper} }

// eventID is an Event's feed id: its annotation, else (an Event recorded
// before ids) its time.
func eventID(ev *corev1.Event) string {
	if id := ev.Annotations[idAnnotation]; id != "" {
		return id
	}
	return feed.ID(ev.EventTime.UnixNano())
}

// nextID is the id of the next Event: its time in nanoseconds, or one
// above the last id when the clock has not moved past it, so ids only grow.
// The first call reads the highest id of the Events kept: a restart
// continues from them.
func (s *Store) nextID(ctx context.Context, at time.Time) (string, error) {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	if !s.idsRead {
		list := &corev1.EventList{}
		if err := s.c.List(ctx, list, client.MatchingLabels{managedBy: beekeeper}); err != nil {
			return "", err
		}
		for i := range list.Items {
			id, err := strconv.ParseInt(eventID(&list.Items[i]), 10, 64)
			if err == nil && id > s.lastID {
				s.lastID = id
			}
		}
		s.idsRead = true
	}
	s.lastID = max(at.UnixNano(), s.lastID+1)
	return feed.ID(s.lastID), nil
}

const (
	managedBy    = "app.kubernetes.io/managed-by"
	beekeeper    = "beekeeper"
	byAnnotation = "beekeeper.giantswarm.io/by"
	idAnnotation = "beekeeper.giantswarm.io/id"
)

// record writes events as Kubernetes Events on obj; the cluster-scoped
// kinds' go to the default namespace.
func (s *Store) record(ctx context.Context, obj client.Object, events []state.Event) error {
	gvk, err := s.c.GroupVersionKindFor(obj)
	if err != nil {
		return err
	}
	ns := obj.GetNamespace()
	if ns == "" {
		ns = metav1.NamespaceDefault
	}
	for _, e := range events {
		by, err := json.Marshal(apiParty(e.By))
		if err != nil {
			return err
		}
		at := e.At
		if at.IsZero() {
			at = time.Now()
		}
		id, err := s.nextID(ctx, at)
		if err != nil {
			return fmt.Errorf("recording %s on %s: the feed id: %w", e.Verb, obj.GetName(), err)
		}
		ev := &corev1.Event{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   ns,
				Name:        fmt.Sprintf("%s.%x", obj.GetName(), at.UnixNano()),
				Labels:      map[string]string{managedBy: beekeeper},
				Annotations: map[string]string{byAnnotation: string(by), idAnnotation: id},
			},
			InvolvedObject: corev1.ObjectReference{
				APIVersion:      gvk.GroupVersion().String(),
				Kind:            gvk.Kind,
				Namespace:       obj.GetNamespace(),
				Name:            obj.GetName(),
				UID:             obj.GetUID(),
				ResourceVersion: obj.GetResourceVersion(),
			},
			Reason:              e.Verb,
			Message:             e.Detail,
			Type:                corev1.EventTypeNormal,
			EventTime:           metav1.MicroTime{Time: at},
			ReportingController: "beekeeper.giantswarm.io/serve",
			ReportingInstance:   e.By.Host,
			Action:              e.Verb,
		}
		if ev.ReportingInstance == "" {
			ev.ReportingInstance = beekeeper
		}
		if err := s.c.Create(ctx, ev); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("recording %s on %s: %w", e.Verb, obj.GetName(), err)
		}
	}
	return nil
}

// snapshot is every object of the shared state as one read saw it.
type snapshot struct {
	envs   map[string]*v1alpha1.Environment
	lanes  map[string]*v1alpha1.MergeLane
	holds  map[string]*v1alpha1.Hold
	notes  map[string]*v1alpha1.Note
	roster map[string]*v1alpha1.RosterEntry
}

func (s *Store) load(ctx context.Context) (*snapshot, error) {
	snap := &snapshot{
		envs: map[string]*v1alpha1.Environment{}, lanes: map[string]*v1alpha1.MergeLane{},
		holds: map[string]*v1alpha1.Hold{}, notes: map[string]*v1alpha1.Note{},
		roster: map[string]*v1alpha1.RosterEntry{},
	}
	envs := &v1alpha1.EnvironmentList{}
	lanes := &v1alpha1.MergeLaneList{}
	holds := &v1alpha1.HoldList{}
	notes := &v1alpha1.NoteList{}
	roster := &v1alpha1.RosterEntryList{}
	for _, l := range []client.ObjectList{envs, lanes, holds, notes, roster} {
		if err := s.c.List(ctx, l); err != nil {
			return nil, err
		}
	}
	for i := range envs.Items {
		snap.envs[envs.Items[i].Name] = &envs.Items[i]
	}
	for i := range lanes.Items {
		snap.lanes[lanes.Items[i].Name] = &lanes.Items[i]
	}
	for i := range holds.Items {
		snap.holds[holds.Items[i].Name] = &holds.Items[i]
	}
	for i := range notes.Items {
		snap.notes[key(&notes.Items[i])] = &notes.Items[i]
	}
	for i := range roster.Items {
		snap.roster[key(&roster.Items[i])] = &roster.Items[i]
	}
	return snap, nil
}

func key(o client.Object) string { return o.GetNamespace() + "/" + o.GetName() }

// state renders the snapshot as the state document, in a canonical order.
func (snap *snapshot) state() *state.State {
	st := &state.State{}
	for _, name := range sortedKeys(snap.envs) {
		env := snap.envs[name]
		for _, g := range env.Status.Queue {
			st.Grants = append(st.Grants, state.Grant{Resource: name, To: party(g.To), By: party(g.By), At: g.At.Time, UpgradeUnblock: g.UpgradeUnblock})
		}
		if env.Status.Released != nil {
			if st.Released == nil {
				st.Released = map[string]time.Time{}
			}
			st.Released[name] = env.Status.Released.Time
		}
	}
	for _, name := range sortedKeys(snap.lanes) {
		for _, e := range snap.lanes[name].Status.Queue {
			st.Merges = append(st.Merges, state.Merge{
				Repo: e.Repo, PR: e.PR, Lane: name, By: party(e.By), Phase: e.Phase, Joined: e.Arrived.Time,
				Seen: timeOf(e.Seen), Started: timeOf(e.Started), Finished: timeOf(e.Finished), Exit: e.Exit, Release: e.Release, Roll: e.Roll,
			})
		}
	}
	for _, name := range sortedKeys(snap.holds) {
		h := snap.holds[name]
		hold := state.Hold{Target: h.Spec.Target, Reason: h.Spec.Reason, By: party(h.Status.By), At: h.Status.At.Time,
			Until: timeOf(h.Spec.Until), Except: h.Spec.Except, UpgradeTo: h.Spec.UpgradeTo, LiftedAt: timeOf(h.Status.LiftedAt)}
		if h.Status.LiftedBy != nil {
			p := party(*h.Status.LiftedBy)
			hold.LiftedBy = &p
		}
		st.Holds = append(st.Holds, hold)
	}
	for _, k := range sortedKeys(snap.notes) {
		n := snap.notes[k]
		st.Notes = append(st.Notes, state.Note{ID: n.Spec.ID, For: n.Spec.For, Text: n.Spec.Text, Due: timeOf(n.Spec.Due),
			Default: n.Spec.Default, By: party(n.Status.By), At: n.Status.At.Time, Fired: timeOf(n.Status.Fired),
			Kind: n.Spec.Kind, Until: n.Spec.Until, Pinned: n.Spec.Pinned, Refs: n.Spec.Refs,
			Question: n.Spec.Question, StatusQuo: n.Spec.StatusQuo, Options: n.Spec.Options, Recommend: n.Spec.Recommend, Posted: n.Status.Posted})
		st.NextNote = max(st.NextNote, n.Spec.ID)
	}
	for _, k := range sortedKeys(snap.roster) {
		r := snap.roster[k]
		st.Agents = append(st.Agents, state.Agent{Party: party(r.Spec.Party), Registered: r.Spec.Registered.Time,
			Task: r.Status.Task, IdleSince: timeOf(r.Status.IdleSince), Done: r.Status.State == "ended", Conversation: r.Status.Conversation})
	}
	canonical(st)
	return st
}

// canonical orders the document the way state renders it: grants and
// merges by their Environment and lane (keeping each one's queue order),
// holds by object name, notes by id, agents by namespace and name.
func canonical(st *state.State) {
	slices.SortStableFunc(st.Grants, func(a, b state.Grant) int { return strings.Compare(a.Resource, b.Resource) })
	slices.SortStableFunc(st.Merges, func(a, b state.Merge) int { return strings.Compare(a.Lane, b.Lane) })
	slices.SortStableFunc(st.Holds, func(a, b state.Hold) int { return strings.Compare(holdName(a.Target), holdName(b.Target)) })
	slices.SortStableFunc(st.Notes, func(a, b state.Note) int { return a.ID - b.ID })
	slices.SortStableFunc(st.Agents, func(a, b state.Agent) int {
		return strings.Compare(teamNamespace(a.Team)+"/"+rosterName(a.Party), teamNamespace(b.Team)+"/"+rosterName(b.Party))
	})
}

// apply returns the objects st describes: the snapshot's own, changed where
// st changed them, new ones for new records, and none for removed ones.
func (snap *snapshot) apply(st *state.State) (*snapshot, error) {
	want := &snapshot{
		envs: map[string]*v1alpha1.Environment{}, lanes: map[string]*v1alpha1.MergeLane{},
		holds: map[string]*v1alpha1.Hold{}, notes: map[string]*v1alpha1.Note{},
		roster: map[string]*v1alpha1.RosterEntry{},
	}
	for name, env := range snap.envs {
		env = env.DeepCopy()
		env.Status.Queue, env.Status.Released = nil, nil
		want.envs[name] = env
	}
	for _, g := range st.Grants {
		env, ok := want.envs[g.Resource]
		if !ok {
			return nil, fmt.Errorf("a grant of %s: %s is not an Environment on this installation", g.Resource, g.Resource)
		}
		env.Status.Queue = append(env.Status.Queue, v1alpha1.Grant{To: apiParty(g.To), By: apiParty(g.By), At: metav1.Time{Time: g.At}, UpgradeUnblock: g.UpgradeUnblock})
	}
	for res, at := range st.Released {
		env, ok := want.envs[res]
		if !ok {
			return nil, fmt.Errorf("the release of %s: %s is not an Environment on this installation", res, res)
		}
		env.Status.Released = &metav1.Time{Time: at}
	}
	for name, env := range want.envs {
		if len(env.Status.Queue) > v1alpha1.MaxQueue {
			return nil, fmt.Errorf("%s: %d grants queued, at most %d", name, len(env.Status.Queue), v1alpha1.MaxQueue)
		}
		env.Status.QueueLength = len(env.Status.Queue)
	}

	for name, lane := range snap.lanes {
		lane = lane.DeepCopy()
		lane.Status.Queue, lane.Status.Running, lane.Status.Settling = nil, "", ""
		want.lanes[name] = lane
	}
	for _, m := range st.Merges {
		lane, ok := want.lanes[m.Lane]
		if !ok {
			return nil, fmt.Errorf("the merge of %s: %s is not a MergeLane on this installation", m.Key(), m.Lane)
		}
		lane.Status.Queue = append(lane.Status.Queue, v1alpha1.LaneEntry{By: apiParty(m.By), Repo: m.Repo, PR: m.PR, Phase: m.Phase,
			Arrived: metav1.Time{Time: m.Joined}, Seen: apiTime(m.Seen), Started: apiTime(m.Started), Finished: apiTime(m.Finished),
			Exit: m.Exit, Release: m.Release, Roll: m.Roll})
		switch m.Phase {
		case state.Running:
			lane.Status.Running = m.Key()
		case state.Settling:
			lane.Status.Settling = m.Key()
		}
	}
	for name, lane := range want.lanes {
		if len(lane.Status.Queue) > v1alpha1.MaxQueue {
			return nil, fmt.Errorf("%s: %d merges queued, at most %d", name, len(lane.Status.Queue), v1alpha1.MaxQueue)
		}
		lane.Status.QueueLength = len(lane.Status.Queue)
	}

	now := time.Now()
	for _, h := range st.Holds {
		name := holdName(h.Target)
		if _, dup := want.holds[name]; dup {
			return nil, fmt.Errorf("two holds on %s", h.Target)
		}
		hold := &v1alpha1.Hold{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if cur, ok := snap.holds[name]; ok {
			hold = cur.DeepCopy()
		}
		hold.Spec.Target, hold.Spec.Reason, hold.Spec.Until = h.Target, h.Reason, apiTime(h.Until)
		hold.Spec.Except, hold.Spec.UpgradeTo = h.Except, h.UpgradeTo
		hold.Status.By, hold.Status.At, hold.Status.LiftedAt = apiParty(h.By), metav1.Time{Time: h.At}, apiTime(h.LiftedAt)
		hold.Status.LiftedBy = nil
		if h.LiftedBy != nil {
			p := apiParty(*h.LiftedBy)
			hold.Status.LiftedBy = &p
		}
		hold.Status.Active = h.Active(now)
		want.holds[name] = hold
	}

	for _, n := range st.Notes {
		if n.By.Team == "" {
			return nil, fmt.Errorf("note #%d: a shared note needs its filer's team", n.ID)
		}
		note := &v1alpha1.Note{ObjectMeta: metav1.ObjectMeta{Namespace: teamNamespace(n.By.Team), Name: "note-" + strconv.Itoa(n.ID)}}
		k := key(note)
		if _, dup := want.notes[k]; dup {
			return nil, fmt.Errorf("two notes #%d", n.ID)
		}
		if cur, ok := snap.notes[k]; ok {
			note = cur.DeepCopy()
		}
		note.Spec.ID, note.Spec.For, note.Spec.Text, note.Spec.Due = n.ID, n.For, n.Text, apiTime(n.Due)
		note.Spec.Default, note.Spec.Kind, note.Spec.Until, note.Spec.Pinned, note.Spec.Refs = n.Default, n.Kind, n.Until, n.Pinned, n.Refs
		note.Spec.Question, note.Spec.StatusQuo, note.Spec.Options, note.Spec.Recommend = n.Question, n.StatusQuo, n.Options, n.Recommend
		note.Status.By, note.Status.At, note.Status.Fired, note.Status.Posted = apiParty(n.By), metav1.Time{Time: n.At}, apiTime(n.Fired), n.Posted
		if note.Status.State == "" {
			note.Status.State = "open"
		}
		want.notes[k] = note
	}

	for _, a := range st.Agents {
		if a.Team == "" || a.Host == "" {
			return nil, fmt.Errorf("agent %s: a roster entry needs its team and host", a.Name)
		}
		r := &v1alpha1.RosterEntry{ObjectMeta: metav1.ObjectMeta{Namespace: teamNamespace(a.Team), Name: rosterName(a.Party)}}
		k := key(r)
		if _, dup := want.roster[k]; dup {
			return nil, fmt.Errorf("two agents %s on %s", a.Name, a.Host)
		}
		if cur, ok := snap.roster[k]; ok {
			r = cur.DeepCopy()
		}
		r.Spec.Address, r.Spec.Party, r.Spec.Registered = "local:"+a.Host+"/"+a.Name, apiParty(a.Party), metav1.Time{Time: a.Registered}
		r.Status.Task, r.Status.IdleSince, r.Status.Conversation = a.Task, apiTime(a.IdleSince), a.Conversation
		switch {
		case a.Done:
			r.Status.State = "ended"
		case a.Task != "":
			r.Status.State = "busy"
		default:
			r.Status.State = "idle"
		}
		want.roster[k] = r
	}

	for name := range want.envs {
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			return nil, fmt.Errorf("environment %q: %s", name, strings.Join(errs, "; "))
		}
	}
	return want, nil
}

// homeless refuses an update that changed what the resources cannot carry:
// had compares the document fn left with the one its objects render.
func homeless(had, kept *state.State) error {
	had = clone(had)
	canonical(had)
	// The next note number follows from the notes present.
	had.NextNote = kept.NextNote
	a, err := members(had)
	if err != nil {
		return err
	}
	b, err := members(kept)
	if err != nil {
		return err
	}
	var lost []string
	for k, v := range a {
		if !bytes.Equal(v, b[k]) {
			lost = append(lost, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			lost = append(lost, k)
		}
	}
	if len(lost) == 0 {
		return nil
	}
	sort.Strings(lost)
	return fmt.Errorf("the update changes %s, which the shared resources do not carry: %w", strings.Join(lost, ", "), ErrLocal)
}

func clone(st *state.State) *state.State {
	raw, _ := json.Marshal(st)
	out := &state.State{}
	_ = json.Unmarshal(raw, out)
	return out
}

func members(st *state.State) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	m := map[string]json.RawMessage{}
	return m, json.Unmarshal(raw, &m)
}

// change is one object an update writes: created, deleted, or its spec or
// status replaced.
type change struct {
	obj            client.Object
	create, delete bool
	spec, status   bool
}

func (c change) ref() string {
	if c.obj.GetNamespace() != "" {
		return fmt.Sprintf("%T %s", c.obj, key(c.obj))
	}
	return fmt.Sprintf("%T %s", c.obj, c.obj.GetName())
}

// diff lists the objects want changed against the snapshot.
func (snap *snapshot) diff(want *snapshot) []change {
	var out []change
	add := func(had, got client.Object, hadSpec, gotSpec, hadStatus, gotStatus any) {
		switch {
		case had == nil:
			out = append(out, change{obj: got, create: true, status: true})
		case got == nil:
			out = append(out, change{obj: had, delete: true})
		default:
			c := change{obj: got, spec: !equal(hadSpec, gotSpec), status: !equal(hadStatus, gotStatus)}
			if c.spec || c.status {
				out = append(out, c)
			}
		}
	}
	for _, k := range union(snap.envs, want.envs) {
		had, got := snap.envs[k], want.envs[k]
		add(objOrNil(had), objOrNil(got), specOf(had), specOf(got), statusOf(had), statusOf(got))
	}
	for _, k := range union(snap.lanes, want.lanes) {
		had, got := snap.lanes[k], want.lanes[k]
		add(objOrNil(had), objOrNil(got), specOf(had), specOf(got), statusOf(had), statusOf(got))
	}
	for _, k := range union(snap.holds, want.holds) {
		had, got := snap.holds[k], want.holds[k]
		add(objOrNil(had), objOrNil(got), specOf(had), specOf(got), statusOf(had), statusOf(got))
	}
	for _, k := range union(snap.notes, want.notes) {
		had, got := snap.notes[k], want.notes[k]
		add(objOrNil(had), objOrNil(got), specOf(had), specOf(got), statusOf(had), statusOf(got))
	}
	for _, k := range union(snap.roster, want.roster) {
		had, got := snap.roster[k], want.roster[k]
		add(objOrNil(had), objOrNil(got), specOf(had), specOf(got), statusOf(had), statusOf(got))
	}
	return out
}

// write applies one change under the resourceVersion read. A created object
// gets its status in a second write, since the API server drops the status
// of a create.
func (s *Store) write(ctx context.Context, c change) (client.Object, error) {
	switch {
	case c.delete:
		return c.obj, s.c.Delete(ctx, c.obj, client.Preconditions{ResourceVersion: ptr(c.obj.GetResourceVersion())})
	case c.create:
		status := statusCopy(c.obj)
		if err := s.c.Create(ctx, c.obj); err != nil {
			return nil, err
		}
		restoreStatus(c.obj, status)
		setObserved(c.obj)
		return c.obj, s.c.Status().Update(ctx, c.obj)
	}
	if c.spec {
		status := statusCopy(c.obj)
		if err := s.c.Update(ctx, c.obj); err != nil {
			return nil, err
		}
		restoreStatus(c.obj, status)
	}
	if c.status || c.spec {
		setObserved(c.obj)
		if err := s.c.Status().Update(ctx, c.obj); err != nil {
			return nil, err
		}
	}
	return c.obj, nil
}

func ptr[T any](v T) *T { return &v }

// statusCopy and restoreStatus carry an object's status across a write of
// its spec, which answers with the status stored.
func statusCopy(o client.Object) any {
	switch o := o.(type) {
	case *v1alpha1.Environment:
		return *o.Status.DeepCopy()
	case *v1alpha1.MergeLane:
		return *o.Status.DeepCopy()
	case *v1alpha1.Hold:
		return *o.Status.DeepCopy()
	case *v1alpha1.Note:
		return *o.Status.DeepCopy()
	case *v1alpha1.RosterEntry:
		return *o.Status.DeepCopy()
	}
	return nil
}

func restoreStatus(o client.Object, status any) {
	switch o := o.(type) {
	case *v1alpha1.Environment:
		o.Status = status.(v1alpha1.EnvironmentStatus)
	case *v1alpha1.MergeLane:
		o.Status = status.(v1alpha1.MergeLaneStatus)
	case *v1alpha1.Hold:
		o.Status = status.(v1alpha1.HoldStatus)
	case *v1alpha1.Note:
		o.Status = status.(v1alpha1.NoteStatus)
	case *v1alpha1.RosterEntry:
		o.Status = status.(v1alpha1.RosterEntryStatus)
	}
}

func setObserved(o client.Object) {
	g := o.GetGeneration()
	switch o := o.(type) {
	case *v1alpha1.Environment:
		o.Status.ObservedGeneration = g
	case *v1alpha1.MergeLane:
		o.Status.ObservedGeneration = g
	case *v1alpha1.Hold:
		o.Status.ObservedGeneration = g
	case *v1alpha1.Note:
		o.Status.ObservedGeneration = g
	case *v1alpha1.RosterEntry:
		o.Status.ObservedGeneration = g
	}
}

// specOf and statusOf return what diff compares; the observed generation is
// the write's own bookkeeping, not a change.
func specOf(o any) any {
	switch o := o.(type) {
	case *v1alpha1.Environment:
		if o != nil {
			return o.Spec
		}
	case *v1alpha1.MergeLane:
		if o != nil {
			return o.Spec
		}
	case *v1alpha1.Hold:
		if o != nil {
			return o.Spec
		}
	case *v1alpha1.Note:
		if o != nil {
			return o.Spec
		}
	case *v1alpha1.RosterEntry:
		if o != nil {
			return o.Spec
		}
	}
	return nil
}

func statusOf(o any) any {
	switch o := o.(type) {
	case *v1alpha1.Environment:
		if o != nil {
			s := *o.Status.DeepCopy()
			s.ObservedGeneration = 0
			return s
		}
	case *v1alpha1.MergeLane:
		if o != nil {
			s := *o.Status.DeepCopy()
			s.ObservedGeneration = 0
			return s
		}
	case *v1alpha1.Hold:
		if o != nil {
			s := *o.Status.DeepCopy()
			s.ObservedGeneration = 0
			return s
		}
	case *v1alpha1.Note:
		if o != nil {
			s := *o.Status.DeepCopy()
			s.ObservedGeneration = 0
			return s
		}
	case *v1alpha1.RosterEntry:
		if o != nil {
			s := *o.Status.DeepCopy()
			s.ObservedGeneration = 0
			return s
		}
	}
	return nil
}

// objOrNil turns a typed nil pointer into a nil interface.
func objOrNil[T any, P interface {
	*T
	client.Object
}](o P) client.Object {
	if o == nil {
		return nil
	}
	return o
}

// equal compares as the API server stores: JSON, times to the second.
func equal(a, b any) bool {
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return bytes.Equal(ra, rb)
}

func union[V any](a, b map[string]V) []string {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	return sortedKeys(keys)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// teamNamespace is a team's namespace, beekeeper-<team>.
func teamNamespace(team string) string { return "beekeeper-" + dnsLabel(team) }

// holdName names a target's Hold: the target made a DNS label, with a hash
// that keeps owner/repo and owner-repo apart.
func holdName(target string) string {
	sum := sha256.Sum256([]byte(target))
	return dnsLabel(target) + "-" + hex.EncodeToString(sum[:4])
}

// rosterName names an agent's RosterEntry by its host and name.
func rosterName(p state.Party) string {
	sum := sha256.Sum256([]byte(p.Host + "/" + p.Name))
	return dnsLabel(p.Host+"-"+p.Name) + "-" + hex.EncodeToString(sum[:4])
}

var notLabel = regexp.MustCompile(`[^a-z0-9-]+`)

func dnsLabel(s string) string {
	s = strings.Trim(notLabel.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		s = "x"
	}
	return s
}

// PartyOf is p as the state document's party.
func PartyOf(p v1alpha1.Party) state.Party { return party(p) }

// APIParty is p as the resources' party.
func APIParty(p state.Party) v1alpha1.Party { return apiParty(p) }

func party(p v1alpha1.Party) state.Party {
	return state.Party{Session: p.Session, HostSession: p.HostSession, Name: p.Name, Person: p.Person, Team: p.Team, Host: p.Host}
}

func apiParty(p state.Party) v1alpha1.Party {
	return v1alpha1.Party{Session: p.Session, HostSession: p.HostSession, Name: p.Name, Person: p.Person, Team: p.Team, Host: p.Host}
}

func timeOf(t *metav1.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.Time
}

func apiTime(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	return &metav1.Time{Time: t}
}
