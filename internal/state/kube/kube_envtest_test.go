package kube

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

// envtestStore starts a kube-apiserver with beekeeper's CRDs and returns
// two stores over it, as two replicas or two writers would hold them. It
// skips without KUBEBUILDER_ASSETS (make test-envtest sets it).
func envtestStore(t *testing.T) (*Store, *Store, client.Client) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run make test-envtest")
	}
	_, file, _, _ := runtime.Caller(0)
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(filepath.Dir(file), "..", "..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	a, err := NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a, b, a.c
}

func environment(t *testing.T, c client.Client, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := c.Create(context.Background(), &v1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       v1alpha1.EnvironmentSpec{Installation: name, Team: team},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func namespace(t *testing.T, c client.Client, team string) {
	t.Helper()
	if err := c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: teamNamespace(team)}}); err != nil {
		t.Fatal(err)
	}
}

const (
	team     = "bumblebee"
	graveler = "graveler"
	muster   = "giantswarm/muster"
)

var (
	ana = state.Party{Session: "s-ana", Name: "Agent one", Person: "ana@example.com", Team: team, Host: "lab"}
	bo  = state.Party{Session: "s-bo", Name: "Agent two", Person: "bo@example.com", Team: team, Host: "laptop"}
)

func TestEnvtestPrinterColumns(t *testing.T) {
	_, _, c := envtestStore(t)
	s := c.Scheme()
	if err := apiextensionsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "environments.beekeeper.giantswarm.io"}, crd); err != nil {
		t.Fatal(err)
	}
	var cols []string
	for _, col := range crd.Spec.Versions[0].AdditionalPrinterColumns {
		cols = append(cols, col.Name)
	}
	if got, want := strings.Join(cols, ","), "Holder,Purpose,Since,Upgrade,Queue"; !strings.HasPrefix(got, want) {
		t.Errorf("printer columns %s, want %s", got, want)
	}
	if crd.Spec.Versions[0].Subresources == nil || crd.Spec.Versions[0].Subresources.Status == nil {
		t.Error("environments have no status subresource")
	}
	if crd.Spec.Scope != apiextensionsv1.ClusterScoped {
		t.Errorf("environments are %s", crd.Spec.Scope)
	}
}

// Of concurrent claims of one Environment, from two stores, exactly one
// wins and every other is told the winner.
func TestEnvtestConcurrentClaims(t *testing.T) {
	a, b, c := envtestStore(t)
	environment(t, c, graveler)
	const claims = 8
	var wg sync.WaitGroup
	errs := make([]error, claims)
	for i := range claims {
		wg.Go(func() {
			s, p := a, ana
			if i%2 == 1 {
				s, p = b, bo
			}
			p.Session += "-" + strconv.Itoa(i)
			p.Name += " " + strconv.Itoa(i)
			errs[i] = s.Claim(graveler, v1alpha1.Holder{Party: apiParty(p), Purpose: "proof", Since: metav1.Now()})
		})
	}
	wg.Wait()
	h, err := a.Holder(graveler)
	if err != nil || h == nil {
		t.Fatalf("holder %v, %v", h, err)
	}
	won := 0
	for i, err := range errs {
		var held *HeldError
		switch {
		case err == nil:
			won++
		case errors.As(err, &held):
			if held.Holder.Party.Name != h.Party.Name {
				t.Errorf("claim %d told %s holds it, the holder is %s", i, held.Holder.Party.Name, h.Party.Name)
			}
		default:
			t.Errorf("claim %d: %v", i, err)
		}
	}
	if won != 1 {
		t.Errorf("%d claims won, want 1", won)
	}

	var held *HeldError
	if err := a.Release(graveler, state.Party{Session: "s-stranger", Name: "stranger"}, time.Now()); !errors.As(err, &held) {
		t.Errorf("a release by someone else than the holder: %v", err)
	}
	if err := a.Release(graveler, party(h.Party), time.Now()); err != nil {
		t.Fatal(err)
	}
	if h, _ := b.Holder(graveler); h != nil {
		t.Errorf("released, still held by %s", h.Party.Name)
	}
	if err := a.Claim("nowhere", v1alpha1.Holder{Party: apiParty(ana), Purpose: "x", Since: metav1.Now()}); err == nil ||
		!strings.Contains(err.Error(), "not an Environment") {
		t.Errorf("claim of an unknown Environment: %v", err)
	}
}

// Concurrent updates of one Environment both land: the loser reruns on the
// winner's state.
func TestEnvtestConcurrentUpdates(t *testing.T) {
	a, b, c := envtestStore(t)
	environment(t, c, graveler, "glean")
	var wg sync.WaitGroup
	for i, s := range []*Store{a, b, a, b} {
		wg.Go(func() {
			to := ana
			to.Name = "worker " + strconv.Itoa(i)
			if err := s.Update(func(st *state.State) ([]state.Event, error) {
				st.Grants = append(st.Grants, state.Grant{Resource: graveler, To: to, By: bo, At: time.Now()})
				return []state.Event{{At: time.Now(), By: bo, Verb: "lease.grant", Detail: "graveler to " + to.Name}}, nil
			}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	st, err := b.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Grants) != 4 {
		t.Fatalf("%d grants, want 4: %+v", len(st.Grants), st.Grants)
	}
	env := &v1alpha1.Environment{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: graveler}, env); err != nil {
		t.Fatal(err)
	}
	if env.Status.QueueLength != 4 || env.Status.ObservedGeneration != env.Generation {
		t.Errorf("queue length %d, observed generation %d of %d", env.Status.QueueLength, env.Status.ObservedGeneration, env.Generation)
	}
	events, err := a.Events(0, func(e state.Event) bool { return e.Verb == "lease.grant" })
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0].By.Person != bo.Person {
		t.Errorf("events %+v", events)
	}
}

func TestEnvtestRecords(t *testing.T) {
	a, _, c := envtestStore(t)
	environment(t, c, graveler)
	namespace(t, c, team)
	for _, l := range []string{graveler} {
		if err := c.Create(context.Background(), &v1alpha1.MergeLane{ObjectMeta: metav1.ObjectMeta{Name: l},
			Spec: v1alpha1.MergeLaneSpec{Installation: l, Repositories: []string{muster}}}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Truncate(time.Second)
	steps := []func(*state.State){
		func(st *state.State) {
			st.Holds = append(st.Holds, state.Hold{Target: muster, Reason: "upgrade", By: ana, At: now, Until: now.Add(time.Hour)})
		},
		func(st *state.State) {
			st.NextNote++
			st.Notes = append(st.Notes, state.Note{ID: st.NextNote, For: "Timo", Text: "go?", By: ana, At: now, Kind: "decision", Refs: []string{"giantswarm/beekeeper#152"}})
		},
		func(st *state.State) {
			st.Merges = append(st.Merges, state.Merge{Repo: muster, PR: 7, Lane: graveler, By: ana, Phase: state.Running, Joined: now, Started: now})
		},
		func(st *state.State) {
			st.Agents = append(st.Agents, state.Agent{Party: bo, Registered: now, Task: "board pull"})
		},
		func(st *state.State) { st.Released = map[string]time.Time{graveler: now} },
	}
	for i, step := range steps {
		if err := a.Update(func(st *state.State) ([]state.Event, error) { step(st); return nil, nil }); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	st, err := a.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Holds) != 1 || !st.Holds[0].Until.Equal(now.Add(time.Hour)) || st.Holds[0].By.Person != ana.Person {
		t.Errorf("holds %+v", st.Holds)
	}
	if len(st.Notes) != 1 || st.Notes[0].ID != 1 || st.NextNote != 1 || st.Notes[0].Refs[0] != "giantswarm/beekeeper#152" {
		t.Errorf("notes %+v, next %d", st.Notes, st.NextNote)
	}
	if len(st.Merges) != 1 || st.Merges[0].Key() != "giantswarm/muster#7" {
		t.Errorf("merges %+v", st.Merges)
	}
	if len(st.Agents) != 1 || st.Agents[0].Task != "board pull" || st.Agents[0].Host != "laptop" {
		t.Errorf("agents %+v", st.Agents)
	}
	if !st.Released[graveler].Equal(now) {
		t.Errorf("released %v", st.Released)
	}
	lane := &v1alpha1.MergeLane{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: graveler}, lane); err != nil {
		t.Fatal(err)
	}
	if lane.Status.Running != "giantswarm/muster#7" || lane.Status.QueueLength != 1 {
		t.Errorf("lane status %+v", lane.Status)
	}
	notes := &v1alpha1.NoteList{}
	if err := c.List(context.Background(), notes, client.InNamespace("beekeeper-bumblebee")); err != nil || len(notes.Items) != 1 || notes.Items[0].Status.State != "open" {
		t.Errorf("notes in the team namespace: %+v, %v", notes.Items, err)
	}

	// Removing a record deletes its object.
	if err := a.Update(func(st *state.State) ([]state.Event, error) { st.Notes = nil; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if st, _ := a.Read(); len(st.Notes) != 0 {
		t.Errorf("notes after removal %+v", st.Notes)
	}
}

func TestEnvtestRefusals(t *testing.T) {
	a, _, c := envtestStore(t)
	environment(t, c, graveler, "glean")
	cases := map[string]struct {
		fn   func(*state.State)
		want string
	}{
		"local field": {func(st *state.State) { st.Timers = append(st.Timers, state.Timer{ID: 1, What: "look"}) }, "timers"},
		"local member of a shared record": {func(st *state.State) {
			st.Merges = nil
			st.Grants = append(st.Grants, state.Grant{Resource: graveler, To: ana, By: bo, At: time.Now()})
			st.Supervisor = &state.Supervisor{Party: ana}
		}, "supervisor"},
		"two objects": {func(st *state.State) {
			st.Grants = append(st.Grants, state.Grant{Resource: graveler, To: ana, By: bo, At: time.Now()},
				state.Grant{Resource: "glean", To: ana, By: bo, At: time.Now()})
		}, "one object per update"},
		"unknown environment": {func(st *state.State) {
			st.Grants = append(st.Grants, state.Grant{Resource: "agentlab-1", To: ana, By: bo, At: time.Now()})
		}, "not an Environment"},
		"note without team": {func(st *state.State) {
			st.Notes = append(st.Notes, state.Note{ID: 1, Text: "x", By: state.Party{Name: "nobody"}, At: time.Now()})
		}, "filer's team"},
		"queue cap": {func(st *state.State) {
			for range v1alpha1.MaxQueue + 1 {
				st.Grants = append(st.Grants, state.Grant{Resource: graveler, To: ana, By: bo, At: time.Now()})
			}
		}, "at most"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := a.Update(func(st *state.State) ([]state.Event, error) { tc.fn(st); return nil, nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %v, want it to name %q", err, tc.want)
			}
		})
	}
	if st, _ := a.Read(); len(st.Grants) != 0 {
		t.Errorf("a refused update wrote %+v", st.Grants)
	}
	if err := a.Log(state.Event{Verb: "build.run"}); !errors.Is(err, ErrLocal) {
		t.Errorf("Log: %v", err)
	}
	if err := a.Record(state.Event{Verb: "secret.copy"}); !errors.Is(err, ErrLocal) {
		t.Errorf("Record: %v", err)
	}
	if _, err := a.ReadFile("snapshot.json", nil); !errors.Is(err, ErrLocal) {
		t.Errorf("ReadFile: %v", err)
	}
}
