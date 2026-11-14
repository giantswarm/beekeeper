package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/k8senv"
	"github.com/go-logr/logr/testr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/history"
	"github.com/giantswarm/beekeeper/internal/inbox"
	"github.com/giantswarm/beekeeper/internal/observe/observetest"
	"github.com/giantswarm/beekeeper/internal/settle"
)

// The smoke test runs one engine on k8senv against in-memory GitHub, history and inbox doubles. It is
// skipped when the test binaries aren't in ~/.cache/beekeeper/test-deps (make test-deps).

var (
	envOnce sync.Once
	envMgr  k8senv.Manager
	envErr  error
)

func depsDir() string {
	if d := os.Getenv("BEEKEEPER_TEST_DEPS"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "beekeeper", "test-deps")
}

func kubeEnv(t *testing.T) *rest.Config {
	t.Helper()
	deps := depsDir()
	apiservers, _ := filepath.Glob(filepath.Join(deps, "k8s-*", "kube-apiserver"))
	sort.Strings(apiservers)
	if len(apiservers) == 0 {
		t.Skip("no kube-apiserver in " + deps)
	}
	if _, err := os.Stat(filepath.Join(deps, "kine")); err != nil {
		t.Skip("no kine in " + deps)
	}
	envOnce.Do(func() {
		home, _ := os.UserHomeDir()
		data := filepath.Join(home, ".cache", "beekeeper", "k8senv")
		_ = os.MkdirAll(data, 0o750)
		crds, _ := filepath.Abs("../../config/crd")
		envMgr = k8senv.NewManager(k8senv.WithPoolSize(1), k8senv.WithCRDDir(crds),
			k8senv.WithKineBinary(filepath.Join(deps, "kine")), k8senv.WithKubeAPIServerBinary(apiservers[len(apiservers)-1]),
			k8senv.WithBaseDataDir(data), k8senv.WithAcquireTimeout(3*time.Minute))
		envErr = envMgr.Initialize(context.Background())
	})
	if envErr != nil {
		t.Fatalf("k8senv: %v", envErr)
	}
	inst, err := envMgr.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	cfg, err := inst.Config()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Release() })
	return rest.CopyConfig(cfg)
}

func TestMain(m *testing.M) {
	code := m.Run()
	if envMgr != nil {
		_ = envMgr.Shutdown()
	}
	os.Exit(code)
}

// memHistory is an in-memory history.Writer and history.Reader.
type memHistory struct {
	mu        sync.Mutex
	events    []history.Event
	decisions []history.Decision
	durations map[string][]time.Duration
}

func (h *memHistory) Append(evs ...history.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range evs {
		e.ID = int64(len(h.events) + 1)
		h.events = append(h.events, e)
	}
}

func (h *memHistory) Decide(d history.Decision) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, x := range h.decisions {
		if x.Key() == d.Key() {
			return false
		}
	}
	d.ID = int64(len(h.decisions) + 1)
	h.decisions = append(h.decisions, d)
	return true
}

func (h *memHistory) Answer(k history.Key, jev string, conf float64, jevErr, outcome string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.decisions {
		if h.decisions[i].Key() == k {
			h.decisions[i].JEV, h.decisions[i].JEVConfidence, h.decisions[i].JEVError, h.decisions[i].Outcome = jev, conf, jevErr, outcome
		}
	}
}

func (h *memHistory) AddDuration(kind, key string, d time.Duration, _ time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.durations == nil {
		h.durations = map[string][]time.Duration{}
	}
	h.durations[kind+"/"+key] = append(h.durations[kind+"/"+key], d)
}

func (h *memHistory) Pending() int { return 0 }
func (h *memHistory) Dropped() int { return 0 }

func (h *memHistory) Events(_ context.Context, f history.EventFilter) ([]history.Event, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []history.Event
	for _, e := range h.events {
		if (f.Item == "" || e.Item == f.Item) && (f.Change == "" || e.Change == f.Change) && (f.Type == "" || e.Type == f.Type) &&
			(f.Source == "" || e.Source == f.Source) && e.ID > f.AfterID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (h *memHistory) Decisions(_ context.Context, f history.DecisionFilter) ([]history.Decision, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []history.Decision
	for _, d := range h.decisions {
		if (f.Point == "" || d.Point == f.Point) && (f.Subject == "" || d.Subject == f.Subject) && d.ID > f.AfterID {
			out = append(out, d)
		}
	}
	return out, nil
}

func (h *memHistory) Durations(_ context.Context, kind, key string) ([]time.Duration, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration(nil), h.durations[kind+"/"+key]...), nil
}

func (h *memHistory) MaxIDs(context.Context) (int64, int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return int64(len(h.events)), int64(len(h.decisions)), nil
}

func (h *memHistory) DecisionKeys(context.Context) ([]history.Key, error) { return nil, nil }

func (h *memHistory) count(typ, item, change, source string) int {
	evs, _ := h.Events(context.Background(), history.EventFilter{Type: typ, Item: item, Change: change, Source: source})
	return len(evs)
}

// memInbox is an in-memory inbox.Drainer whose deliveries the tracker sees until handled.
type memInbox struct {
	tracker *settle.Tracker
	ch      chan inbox.Delivery
	mu      sync.Mutex
	n       int
}

func (b *memInbox) Put(d inbox.Delivery) {
	b.mu.Lock()
	b.n++
	b.mu.Unlock()
	done := b.tracker.Begin("delivery " + d.ID)
	go func() {
		b.ch <- d
		_ = done
	}()
	b.tracker.AddProbe("inbox", func() (bool, string) {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.n == 0, fmt.Sprintf("%d unprocessed", b.n)
	})
	done()
}

func (b *memInbox) Run(ctx context.Context, h inbox.Handler) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case d := <-b.ch:
			_ = h(ctx, d)
			b.mu.Lock()
			b.n--
			b.mu.Unlock()
			b.tracker.Poke()
		}
	}
}

func (b *memInbox) Unprocessed(context.Context) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n, nil
}

var t0 = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

func fixtureSpec() v1alpha1.InstallationSpec {
	d := func(s string) metav1.Duration { x, _ := time.ParseDuration(s); return metav1.Duration{Duration: x} }
	return v1alpha1.InstallationSpec{
		Org: org, Board: v1alpha1.BoardSpec{Project: 273}, Mode: v1alpha1.ModeDryRun,
		Resync:    v1alpha1.ResyncSpec{Board: d("10m"), Changes: d("15m")},
		Freshness: v1alpha1.FreshnessSpec{DeliveryLog: d("5m"), BoardProbe: d("10m")},
		Polling:   v1alpha1.PollingSpec{Interval: d("5m")},
		Budget:    v1alpha1.BudgetSpec{ObservationReserve: 0.2},
		Retry:     v1alpha1.RetrySpec{Initial: d("10s"), Cap: d("5m")}, ItemRetention: d("168h"),
		JEV: v1alpha1.JEVSpec{ConfidenceFloor: 0.5},
		DecisionPoints: map[string]string{
			"ready?": v1alpha1.DecisionModeShadow, "next?": v1alpha1.DecisionModeShadow,
			"parallel-safe?": v1alpha1.DecisionModeShadow, "bug-severity?": v1alpha1.DecisionModeShadow,
		},
		Waits: v1alpha1.WaitsSpec{
			CI:          v1alpha1.CIWaitSpec{DeadlineFactor: 3, DefaultDeadline: d("1h"), MinHistory: 3, AtDeadline: v1alpha1.AtDeadlineReobserve},
			Review:      v1alpha1.FixedWaitSpec{Deadline: d("48h"), AtDeadline: v1alpha1.AtDeadlineReobserve},
			Dependency:  v1alpha1.FixedWaitSpec{Deadline: d("168h"), AtDeadline: v1alpha1.AtDeadlineReobserve},
			MergeResult: v1alpha1.FixedWaitSpec{Deadline: d("2h"), AtDeadline: v1alpha1.AtDeadlineReobserve},
			Grace:       d("5m"), OverdueBackoffCap: d("10m"),
		},
		Ready: v1alpha1.ReadySpec{AcceptanceCriteria: `(?im)^#{1,4}\s*acceptance criteria`,
			TargetRepo:    `(?im)^(repo|repository):\s*([\w.-]+/[\w.-]+)`,
			PlanningRepos: []string{"giantswarm/roadmap"}, Todo: "refine with `/gs-plans:gs-plan`"},
		Footprint: `(?im)^(footprint|paths):\s*(.+)$`,
	}
}

// Fixtures of the Installation, the board and the forge.
const (
	org        = "giantswarm"
	team       = "Bumblebee🐝"
	bumblebee  = "bumblebee"
	alice      = "alice"
	bob        = "bob"
	story      = "Story 📑"
	repoKlaus  = "giantswarm/klaus"
	refKlaus55 = "giantswarm/klaus#55"
	stateOpen  = "OPEN"

	checkGoBuild = "go-build"
	checkGoTest  = "go-test"
	completed    = "completed"
)

func seedForge(f *observetest.Forge) {
	f.Schema = forge.BoardSchema{ProjectID: "PVT_giantswarm_273", Options: map[string][]string{"Team": {team, "Atlas 🗺️"}}}
	f.Repos = []string{"giantswarm/roadmap", repoKlaus}
	body := "### Acceptance criteria\n- [ ] retries 5xx\nRepo: giantswarm/klaus\n"
	f.Items["PVTI_101"] = forge.BoardItem{NodeID: "PVTI_101", Status: "Up Next ➡️", Kind: story, Team: team, IsIssue: true,
		UpdatedAt: t0.Add(-time.Hour), Issue: forge.Issue{Ref: "giantswarm/roadmap#101", Title: "Retry", Body: body, State: stateOpen,
			CreatedAt: t0.Add(-48 * time.Hour)}}
	f.Items["PVTI_102"] = forge.BoardItem{NodeID: "PVTI_102", Status: "In Progress ⛏️", Kind: story, Team: team, IsIssue: true,
		UpdatedAt: t0.Add(-time.Hour), Issue: forge.Issue{Ref: "giantswarm/roadmap#102", Title: "Metrics", Body: body, State: stateOpen,
			Assignees: []string{alice}, CreatedAt: t0.Add(-72 * time.Hour),
			ClosedBy: []forge.LinkedPR{{Ref: refKlaus55, State: stateOpen}}}}
	f.Items["PVTI_105"] = forge.BoardItem{NodeID: "PVTI_105", Status: "Backlog 📦", Kind: story, Team: team, IsIssue: true,
		Issue: forge.Issue{Ref: "giantswarm/roadmap#105", Title: "Later", State: stateOpen}}
	f.PRs[refKlaus55] = forge.PullRequest{Ref: refKlaus55, State: "open", HeadSHA: "a1", Base: "main", Author: alice}
	f.Checks["giantswarm/klaus@a1"] = []forge.CheckRun{{Name: checkGoBuild, Status: completed, Conclusion: "success"},
		{Name: checkGoTest, Status: "in_progress"}}
	f.Rules[repoKlaus] = forge.BranchRules{RequiredChecks: []string{checkGoBuild, checkGoTest}, RequiredApprovals: 1}
	f.Limits = forge.Budget{REST: forge.APILimit{Limit: 5000, Remaining: 4990, Known: true},
		GraphQL: forge.APILimit{Limit: 5000, Remaining: 4995, Known: true}}
}

type world struct {
	t       *testing.T
	kube    client.Client
	ns      string
	clock   *clock.Fake
	tracker *settle.Tracker
	forge   *observetest.Forge
	hist    *memHistory
	inbox   *memInbox
	eng     *Engine
}

func (w *world) settle() {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.tracker.Wait(ctx, 5*time.Second); err != nil {
		w.t.Fatalf("settle: %v", err)
	}
}

// advance moves the clock by d from due timer to due timer, settling at each instant.
func (w *world) advance(d time.Duration) {
	w.t.Helper()
	limit := w.clock.Now().Add(d)
	for w.clock.Step(limit) {
		w.settle()
	}
	w.clock.Set(limit)
	w.settle()
}

func (w *world) item(ref string) *v1alpha1.Item {
	w.t.Helper()
	it := &v1alpha1.Item{}
	if err := w.kube.Get(context.Background(), client.ObjectKey{Namespace: w.ns, Name: v1alpha1.ItemName(ref)}, it); err != nil {
		w.t.Fatalf("get item %s: %v", ref, err)
	}
	return it
}

func (w *world) change(ref string) *v1alpha1.Change {
	w.t.Helper()
	c := &v1alpha1.Change{}
	if err := w.kube.Get(context.Background(), client.ObjectKey{Namespace: w.ns, Name: v1alpha1.ChangeName(ref)}, c); err != nil {
		w.t.Fatalf("get change %s: %v", ref, err)
	}
	return c
}

func newWorld(t *testing.T) *world {
	cfg := kubeEnv(t)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ns := fmt.Sprintf("smoke-%d", time.Now().UnixNano())
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	objs := []client.Object{
		&v1alpha1.Installation{ObjectMeta: metav1.ObjectMeta{Name: org, Namespace: ns}, Spec: fixtureSpec()},
		&v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: bumblebee, Namespace: ns}, Spec: v1alpha1.TeamSpec{BoardOption: team,
			GithubTeam: "team-bumblebee", OptIn: v1alpha1.OptIn{Work: []string{"implementable"}}}},
		&v1alpha1.Person{ObjectMeta: metav1.ObjectMeta{Name: alice, Namespace: ns}, Spec: v1alpha1.PersonSpec{Login: alice,
			Teams: []string{bumblebee}, WIP: v1alpha1.WIP{Items: 1, Epics: 1}}},
		&v1alpha1.Person{ObjectMeta: metav1.ObjectMeta{Name: bob, Namespace: ns}, Spec: v1alpha1.PersonSpec{Login: bob,
			Teams: []string{bumblebee}, WIP: v1alpha1.WIP{Items: 1, Epics: 1}}},
	}
	for _, o := range objs {
		if err := kube.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	w := &world{t: t, kube: kube, ns: ns, clock: clock.NewFake(t0), tracker: settle.New(), forge: observetest.New(),
		hist: &memHistory{}}
	w.inbox = &memInbox{tracker: w.tracker, ch: make(chan inbox.Delivery, 16)}
	seedForge(w.forge)
	w.eng, err = New(Options{Namespace: ns, Replica: "engine-0", Rest: cfg, Clock: w.clock, Tracker: w.tracker, Forge: w.forge,
		History: w.hist, HistoryReader: w.hist, Inbox: w.inbox, Registry: prometheus.NewRegistry(), Logger: testr.New(t),
		Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	ectx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.eng.Start(ectx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("engine didn't stop")
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for !w.eng.IsLeader() {
		if time.Now().After(deadline) {
			t.Fatal("engine didn't lead")
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.settle()
	return w
}

func TestEngineSmoke(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	// Start-up: the team's active items are Items, the backlog item isn't; the PR is a Change waiting on CI.
	ready := w.item("giantswarm/roadmap#101")
	if ready.Status.Phase != v1alpha1.PhaseReady || ready.Status.Observed == nil || ready.Status.Observed.Source != "startup" {
		t.Fatalf("#101 = %+v", ready.Status)
	}
	if ready.Status.Shadow == nil || ready.Status.Shadow.Pick == nil {
		t.Errorf("#101 not picked: %+v", ready.Status.Shadow)
	}
	inflight := w.item("giantswarm/roadmap#102")
	if inflight.Status.Phase != v1alpha1.PhasePublished {
		t.Errorf("#102 phase = %s", inflight.Status.Phase)
	}
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: v1alpha1.ItemName("giantswarm/roadmap#105")}, &v1alpha1.Item{}); err == nil {
		t.Error("backlog item became an Item")
	}
	ch := w.change(refKlaus55)
	if ch.Status.CI == nil || ch.Status.CI.State != v1alpha1.CIPending || ch.Status.Wait == nil || ch.Status.Wait.Ref != "ci:giantswarm/klaus@a1" {
		t.Fatalf("change = %+v", ch.Status)
	}
	if n := w.hist.count("observed", "giantswarm/roadmap#101", "", "startup"); n != 1 {
		t.Errorf("observed rows for #101 = %d", n)
	}
	decisions, _ := w.hist.Decisions(ctx, history.DecisionFilter{Point: "ready?"})
	if len(decisions) == 0 {
		t.Error("no ready? decision")
	}
	inst := &v1alpha1.Installation{}
	_ = w.kube.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: org}, inst)
	if inst.Status.Leader != "engine-0" || inst.Status.Sources[v1alpha1.SourceWebhooks].State != v1alpha1.SourceFresh {
		t.Errorf("installation status = %+v", inst.Status)
	}
	budget := &v1alpha1.Budget{}
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: v1alpha1.BudgetName}, budget); err != nil || budget.Status.REST.Limit != 5000 {
		t.Errorf("budget = %+v, %v", budget.Status, err)
	}

	// A check_run webhook wakes only the Change on that SHA; CI green, so it waits on review.
	w.forge.Lock()
	w.forge.Checks["giantswarm/klaus@a1"][1] = forge.CheckRun{Name: checkGoTest, Status: completed, Conclusion: "success"}
	w.forge.Unlock()
	payload, _ := json.Marshal(map[string]any{"action": completed, "check_run": map[string]any{"head_sha": "a1"},
		"repository": map[string]any{"full_name": repoKlaus}})
	before := w.eng.Reconciles("Item", "giantswarm/roadmap#101")
	w.inbox.Put(inbox.Delivery{ID: "d1", Event: "check_run", Action: completed, ReceivedAt: t0, Payload: payload})
	w.settle()
	ch = w.change(refKlaus55)
	if ch.Status.CI.State != v1alpha1.CIGreen || ch.Status.Wait == nil || ch.Status.Wait.On != v1alpha1.WaitOnReview {
		t.Fatalf("after check_run: %+v", ch.Status)
	}
	if n := w.hist.count("wait.woken", "", refKlaus55, "webhook"); n != 1 {
		t.Errorf("wait.woken rows = %d", n)
	}
	if n := w.hist.count("webhook.missed", "", "", ""); n != 0 {
		t.Errorf("missed rows = %d", n)
	}
	if after := w.eng.Reconciles("Item", "giantswarm/roadmap#101"); after != before {
		t.Errorf("#101 reconciled by an unrelated webhook (%d → %d)", before, after)
	}

	// An unrelated SHA wakes nothing and reads nothing.
	calls := w.forge.Count()
	total := w.eng.Reconciles("", "")
	payload, _ = json.Marshal(map[string]any{"sha": "f9", "repository": map[string]any{"full_name": repoKlaus}})
	w.inbox.Put(inbox.Delivery{ID: "d2", Event: "status", ReceivedAt: t0, Payload: payload})
	w.settle()
	if w.forge.Count() != calls || w.eng.Reconciles("", "") != total {
		t.Errorf("unrelated status: calls %d → %d, reconciles %d → %d", calls, w.forge.Count(), total, w.eng.Reconciles("", ""))
	}

	// A board change GitHub never announced is caught by the 09:10 resync and counted as missed.
	w.forge.Lock()
	bi := w.forge.Items["PVTI_101"]
	bi.Status, bi.UpdatedAt = "In Progress ⛏️", t0.Add(time.Minute)
	bi.Issue.Assignees = []string{bob}
	w.forge.Items["PVTI_101"] = bi
	w.forge.Unlock()
	writes := w.eng.Writes()
	w.advance(10 * time.Minute)
	it := w.item("giantswarm/roadmap#101")
	if it.Status.Phase != v1alpha1.PhaseImplementing || it.Status.Observed.Source != "resync" {
		t.Errorf("after resync: phase %s observed %+v", it.Status.Phase, it.Status.Observed)
	}
	if n := w.hist.count("webhook.missed", "giantswarm/roadmap#101", "", "resync"); n != 2 {
		t.Errorf("missed rows for #101 = %d, want 2 (status, assignees)", n)
	}
	inst = &v1alpha1.Installation{}
	_ = w.kube.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: org}, inst)
	if inst.Status.Sources[v1alpha1.SourceBoard].State != v1alpha1.SourceDegraded {
		t.Errorf("board source = %+v", inst.Status.Sources[v1alpha1.SourceBoard])
	}
	if w.eng.Writes() == writes {
		t.Error("nothing written")
	}

	// An idle quarter hour: the change resync reads conditionally, and no Item or Change is written
	// (the decision pass of the board resync stamps the Team and the Installation).
	w.advance(20 * time.Minute) // let the board recover first
	itemRV, changeRV := w.item("giantswarm/roadmap#101").ResourceVersion, w.change(refKlaus55).ResourceVersion
	mark := len(w.forge.Calls)
	w.advance(15 * time.Minute)
	for _, c := range w.forge.Calls[mark:] {
		if c.Conditional && !c.NotModified {
			t.Errorf("conditional read changed: %+v", c)
		}
	}
	if w.item("giantswarm/roadmap#101").ResourceVersion != itemRV || w.change(refKlaus55).ResourceVersion != changeRV {
		t.Error("an idle quarter hour wrote the Item or the Change")
	}
	if d, m, err := w.eng.Replay(ctx); err != nil || d == 0 || m != 0 {
		t.Errorf("replay = %d, %d, %v", d, m, err)
	}
}
