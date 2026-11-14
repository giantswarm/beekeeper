// Package controller holds the engine's reconcilers and the leader's observation loop (data flow §3–4
// of the architecture): the Item, Change, Team (decision pass), Installation and Budget reconcilers, and
// the observer, which runs the board and change resyncs, the freshness checks, polling and the
// resolution of board items webhooks name, all on the engine's clock. Only the leader reconciles,
// drains the inbox and schedules; every write goes through a store that the settle tracker watches.
package controller

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/decide"
	"github.com/giantswarm/beekeeper/internal/faults"
	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/history"
	"github.com/giantswarm/beekeeper/internal/inbox"
	"github.com/giantswarm/beekeeper/internal/jev"
	"github.com/giantswarm/beekeeper/internal/metrics"
	"github.com/giantswarm/beekeeper/internal/observe"
	"github.com/giantswarm/beekeeper/internal/settle"
	"github.com/giantswarm/beekeeper/internal/wake"
)

// Annotations on engine-owned objects.
const (
	// AnnotationRef is the issue or PR ref an Item or Change is about (owner/repo#n).
	AnnotationRef = "beekeeper.giantswarm.io/ref"
)

// Config is what the runtime needs.
type Config struct {
	Namespace string
	Replica   string
	Clock     clock.Clock
	// Tracker makes every asynchronous unit visible; nil in production.
	Tracker       *settle.Tracker
	Forge         forge.Reader
	History       history.Writer
	HistoryReader history.Reader
	Inbox         inbox.Drainer
	JEV           jev.Client
	Faults        *faults.Faults
	Metrics       *metrics.Metrics
	Logger        logr.Logger
	// Workers is the number of concurrent Item and Change reconciles (default 4).
	Workers int

	// OnInstallation, when set, is called with the Installation's spec when the leader first sees it and
	// whenever its generation changes (the binary applies the client's reserve and JEV's endpoint).
	OnInstallation func(*v1alpha1.InstallationSpec)

	// DecideItem, DecideChange and DecideTeam default to the decide package; tests replace them.
	DecideItem   func(decide.ItemView) decide.ItemDecision
	DecideChange func(decide.ChangeView) decide.ChangeDecision
	DecideTeam   func(decide.TeamView) decide.TeamDecision
}

// Runtime is one replica's controllers and observation state.
type Runtime struct {
	cfg     Config
	log     logr.Logger
	clock   clock.Clock
	timers  *clock.Queue
	tracker *settle.Tracker
	forge   forge.Reader
	hist    history.Writer
	m       *metrics.Metrics
	ns      string

	store   *store
	pending observe.PendingSet
	rules   *observe.RulesCache
	inputs  *observe.Inputs

	itemQ, changeQ, teamQ, instQ, budgetQ, taskQ *enqueuer

	leading atomic.Bool

	mu   sync.Mutex
	life context.Context // the leader's context
	// startedUp is closed when the leader's start-up sequence has run.
	startedUp   chan struct{}
	leaderSince time.Time
	obs         *observation
	retries     map[string]retryState
	refusalRe   map[string]int
	teamSource  map[string]string
	ciDurations map[string][]time.Duration
	actions     map[string]bool
	busyChanges map[string]bool
	waiters     map[string]map[string]bool
	deliveries  map[string]*heldDelivery
	lastDecide  time.Time
	// decidable is when this leader first saw an Installation and a Team: no decision pass is due before.
	decidable     time.Time
	lastBudget    budgetSig
	budgetWritten bool
	lastConfig    string
	appliedSpec   string
	drained       chan struct{}

	dropped int

	statMu     sync.Mutex
	reconciles map[string]int
}

// retryState is an object's transient-retry backoff, kept in memory.
type retryState struct {
	attempt int
	at      time.Time
}

// New returns the runtime of one replica.
func New(cfg Config) *Runtime {
	if cfg.Tracker == nil {
		cfg.Tracker = settle.New()
	}
	if cfg.Faults == nil {
		cfg.Faults = &faults.Faults{}
	}
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.New(nil)
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.DecideItem == nil {
		cfg.DecideItem = decide.Item
	}
	if cfg.DecideChange == nil {
		cfg.DecideChange = decide.Change
	}
	if cfg.DecideTeam == nil {
		cfg.DecideTeam = decide.Team
	}
	if cfg.Logger.GetSink() == nil {
		cfg.Logger = logr.Discard()
	}
	rt := &Runtime{
		cfg: cfg, log: cfg.Logger, clock: cfg.Clock, tracker: cfg.Tracker, forge: cfg.Forge, hist: cfg.History,
		m: cfg.Metrics, ns: cfg.Namespace,
		timers:     clock.NewQueue(cfg.Clock),
		rules:      &observe.RulesCache{Reader: cfg.Forge, Clock: cfg.Clock},
		inputs:     observe.NewInputs(""),
		reconciles: map[string]int{},
	}
	rt.timers.Stopped = func(key string) bool {
		return cfg.Faults.StopTimers.Load() && len(key) > 5 && key[:5] == "wait/"
	}
	rt.resetState()
	return rt
}

func (rt *Runtime) resetState() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.obs = newObservation()
	rt.retries = map[string]retryState{}
	rt.refusalRe = map[string]int{}
	rt.teamSource = map[string]string{}
	rt.ciDurations = map[string][]time.Duration{}
	rt.actions = map[string]bool{}
	rt.busyChanges = map[string]bool{}
	rt.waiters = map[string]map[string]bool{}
	rt.deliveries = map[string]*heldDelivery{}
	rt.decidable, rt.appliedSpec = time.Time{}, ""
	rt.startedUp = make(chan struct{})
	rt.drained = make(chan struct{}, 1)
}

// Setup registers the indexes, informer hooks, controllers and the leader's runnable with mgr.
func (rt *Runtime) Setup(ctx context.Context, mgr manager.Manager) error {
	rt.store = newStore(mgr.GetClient(), rt.ns, rt.tracker)
	idx := mgr.GetFieldIndexer()
	if err := idx.IndexField(ctx, &v1alpha1.Item{}, wake.IndexRefs, func(o client.Object) []string {
		return wake.ItemRefs(o.(*v1alpha1.Item))
	}); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &v1alpha1.Item{}, wake.IndexChanges, func(o client.Object) []string {
		return o.(*v1alpha1.Item).Status.Changes
	}); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &v1alpha1.Change{}, wake.IndexRefs, func(o client.Object) []string {
		return wake.ChangeRefs(o.(*v1alpha1.Change))
	}); err != nil {
		return err
	}
	kinds := map[string]client.Object{
		KindItem: &v1alpha1.Item{}, KindChange: &v1alpha1.Change{}, KindTeam: &v1alpha1.Team{},
		KindPerson: &v1alpha1.Person{}, KindInstallation: &v1alpha1.Installation{}, KindBudget: &v1alpha1.Budget{},
	}
	for kind, obj := range kinds {
		inf, err := mgr.GetCache().GetInformer(ctx, obj)
		if err != nil {
			return fmt.Errorf("informer for %s: %w", kind, err)
		}
		if _, err := inf.AddEventHandler(rt.store.handler(kind)); err != nil {
			return err
		}
	}

	rt.itemQ = newEnqueuer("item", rt.ns, rt.tracker)
	rt.changeQ = newEnqueuer("change", rt.ns, rt.tracker)
	rt.teamQ = newEnqueuer("team", rt.ns, rt.tracker)
	rt.instQ = newEnqueuer("installation", rt.ns, rt.tracker)
	rt.budgetQ = newEnqueuer("budget", rt.ns, rt.tracker)
	rt.taskQ = newEnqueuer("observer", rt.ns, rt.tracker)

	createDelete := predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true }, DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(event.UpdateEvent) bool { return false }, GenericFunc: func(event.GenericEvent) bool { return true },
	}
	opts := func(name string, workers int) controller.Options {
		return controller.Options{
			MaxConcurrentReconciles: workers,
			SkipNameValidation:      ptr.To(true),
			RecoverPanic:            ptr.To(true),
			NewQueue: func(_ string, rl workqueue.TypedRateLimiter[reconcile.Request]) workqueue.TypedRateLimitingInterface[reconcile.Request] {
				return newTrackedQueue(rt.cfg.Replica+"/"+name, rl, rt.tracker)
			},
		}
	}
	if err := ctrl.NewControllerManagedBy(mgr).Named("item").
		For(&v1alpha1.Item{}, builder.WithPredicates(createDelete)).
		WatchesRawSource(rt.itemQ).
		WithOptions(opts("item", rt.cfg.Workers)).
		Complete(rt.counted(KindItem, rt.reconcileItem)); err != nil {
		return err
	}
	if err := ctrl.NewControllerManagedBy(mgr).Named("change").
		For(&v1alpha1.Change{}, builder.WithPredicates(createDelete)).
		WatchesRawSource(rt.changeQ).
		WithOptions(opts("change", rt.cfg.Workers)).
		Complete(rt.counted(KindChange, rt.reconcileChange)); err != nil {
		return err
	}
	toTeams := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		var reqs []reconcile.Request
		for _, t := range o.(*v1alpha1.Person).Spec.Teams {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKey{Namespace: rt.ns, Name: t}})
		}
		return reqs
	})
	if err := ctrl.NewControllerManagedBy(mgr).Named("team").
		For(&v1alpha1.Team{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1alpha1.Person{}, toTeams, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WatchesRawSource(rt.teamQ).
		WithOptions(opts("team", 1)).
		Complete(rt.counted(KindTeam, rt.reconcileTeam)); err != nil {
		return err
	}
	toInstallation := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: rt.ns, Name: instKey}}}
	})
	if err := ctrl.NewControllerManagedBy(mgr).Named("installation").
		Watches(&v1alpha1.Installation{}, toInstallation, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1alpha1.Team{}, toInstallation, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1alpha1.Person{}, toInstallation, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WatchesRawSource(rt.instQ).
		WithOptions(opts("installation", 1)).
		Complete(rt.counted(KindInstallation, rt.reconcileInstallation)); err != nil {
		return err
	}
	if err := ctrl.NewControllerManagedBy(mgr).Named("budget").
		WatchesRawSource(rt.budgetQ).
		WithOptions(opts("budget", 1)).
		Complete(rt.counted(KindBudget, rt.reconcileBudget)); err != nil {
		return err
	}
	if err := ctrl.NewControllerManagedBy(mgr).Named("observer").
		WatchesRawSource(rt.taskQ).
		WithOptions(opts("observer", 1)).
		Complete(rt.counted("Task", rt.runTask)); err != nil {
		return err
	}
	return mgr.Add(leaderRunnable{rt})
}

// instKey is the installation reconciler's single request: it reconciles the namespace's Installation.
const instKey = "installation"

// counted wraps a reconciler: it counts reconciles for Stats and errors for the metrics.
func (rt *Runtime) counted(kind string, f func(context.Context, reconcile.Request) (reconcile.Result, error)) reconcile.Func {
	return func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
		if !rt.leading.Load() {
			return reconcile.Result{}, nil
		}
		rt.statMu.Lock()
		rt.reconciles[kind+"/"+req.Name]++
		rt.reconciles[kind+"/"]++
		rt.reconciles["/"]++
		rt.statMu.Unlock()
		res, err := f(ctx, req)
		if err != nil {
			rt.m.ReconcileErrors.WithLabelValues(kind).Inc()
			rt.log.Error(err, "reconcile", "kind", kind, "name", req.Name)
		}
		return res, err
	}
}

// Reconciles counts reconciles of one object by this replica: kind Item or Change with an issue or PR
// ref; an empty ref counts every object of the kind, an empty kind every reconcile.
func (rt *Runtime) Reconciles(kind, ref string) int {
	key := kind + "/"
	switch {
	case kind == "":
		key = "/"
	case ref == "":
	case kind == KindItem:
		key += v1alpha1.ItemName(ref)
	case kind == KindChange:
		key += v1alpha1.ChangeName(ref)
	default:
		key += ref
	}
	rt.statMu.Lock()
	defer rt.statMu.Unlock()
	return rt.reconciles[key]
}

// Writes counts object writes by this replica.
func (rt *Runtime) Writes() int {
	if rt.store == nil {
		return 0
	}
	return int(rt.store.writes.Load())
}

// IsLeader reports whether this replica leads (its start-up sequence has begun).
func (rt *Runtime) IsLeader() bool { return rt.leading.Load() }

// leaderRunnable runs the leader's part: start-up, the inbox drain, and stopping cleanly.
type leaderRunnable struct{ rt *Runtime }

func (leaderRunnable) NeedLeaderElection() bool { return true }

func (l leaderRunnable) Start(ctx context.Context) error { return l.rt.lead(ctx) }

// lead runs while this replica leads: it starts up, drains the inbox, and on ctx's end stops every
// timer so a stopped replica fires nothing on a shared clock.
func (rt *Runtime) lead(ctx context.Context) error {
	starting := rt.tracker.Begin(rt.cfg.Replica + " leader start-up")
	rt.resetState()
	rt.store.start()
	now := rt.clock.Now()
	rt.mu.Lock()
	rt.life = ctx
	rt.leaderSince = now
	startedUp, drained := rt.startedUp, rt.drained
	rt.mu.Unlock()
	rt.leading.Store(true)
	prevResume := rt.cfg.Faults.OnResume
	rt.cfg.Faults.OnResume = func() {
		if prevResume != nil {
			prevResume()
		}
		rt.resume()
	}
	if l, ok := rt.hist.(interface{ LoadKeys(context.Context) error }); ok {
		if err := l.LoadKeys(ctx); err != nil {
			rt.log.Error(err, "load decision keys")
		}
	}
	rt.loadActions(ctx)
	rt.taskQ.Add(taskStartup)
	rt.instQ.Add(instKey)

	defer func() {
		rt.leading.Store(false)
		rt.timers.CancelAll()
		rt.pending.Clear()
		rt.store.stop()
		starting()
	}()

	select {
	case <-startedUp:
	case <-ctx.Done():
		return nil
	}
	drainDone := make(chan struct{})
	if rt.cfg.Inbox != nil {
		go func() {
			defer close(drainDone)
			if err := rt.cfg.Inbox.Run(ctx, rt.HandleDelivery); err != nil && ctx.Err() == nil {
				rt.log.Error(err, "inbox drain stopped")
			}
		}()
		// Stay busy until the drain runs: once it has handled a delivery (or there is none), its own
		// probe shows what is left of the deliveries that arrived while no replica led.
		if n, err := rt.cfg.Inbox.Unprocessed(ctx); err == nil && n > 0 {
			select {
			case <-drained:
			case <-ctx.Done():
				return nil
			}
		}
	} else {
		close(drainDone)
	}
	starting()
	<-ctx.Done()
	<-drainDone
	return nil
}

// loadActions rebuilds the set of recorded would-action keys from history.
func (rt *Runtime) loadActions(ctx context.Context) {
	if rt.cfg.HistoryReader == nil {
		return
	}
	evs, err := rt.cfg.HistoryReader.Events(ctx, history.EventFilter{Type: decide.EventShadowAction})
	if err != nil {
		rt.log.Error(err, "load recorded would-actions")
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, e := range evs {
		if k, _ := e.Data["key"].(string); k != "" {
			rt.actions[k] = true
		}
	}
}

// resume fires overdue wait timers at once after the stop_timers fault is lifted.
func (rt *Runtime) resume() {
	if !rt.leading.Load() {
		return
	}
	ctx := rt.context()
	now := rt.clock.Now()
	items, _ := rt.store.items(ctx, nil)
	for _, it := range items {
		rt.rescheduleWait(KindItem, it.Name, it.Status.Wait, now)
	}
	changes, _ := rt.store.changes(ctx, nil)
	for _, c := range changes {
		rt.rescheduleWait(KindChange, c.Name, c.Status.Wait, now)
	}
}

func (rt *Runtime) rescheduleWait(kind, name string, w *v1alpha1.Wait, now time.Time) {
	at, ok := waitNext(w)
	if !ok {
		return
	}
	if !at.After(now) {
		rt.enqueue(kind, name)
		return
	}
	rt.scheduleObject(kind, name, at)
}

func (rt *Runtime) context() context.Context {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.life == nil {
		return context.Background()
	}
	return rt.life
}

func (rt *Runtime) enqueue(kind, name string) {
	switch kind {
	case KindItem:
		rt.itemQ.Add(name)
	case KindChange:
		rt.changeQ.Add(name)
	case KindTeam:
		rt.teamQ.Add(name)
	}
}

// Snapshot is what the metrics' derived gauges and invariants are computed from; the zero Snapshot on
// a replica that doesn't lead.
func (rt *Runtime) Snapshot() metrics.Snapshot {
	rt.syncDropped()
	if !rt.leading.Load() || rt.store == nil {
		return metrics.Snapshot{}
	}
	ctx := rt.context()
	s := metrics.Snapshot{Leader: true, Now: rt.clock.Now()}
	if insts, err := rt.store.installations(ctx); err == nil && len(insts) > 0 {
		s.Installation = insts[0]
	}
	if teams, err := rt.store.teams(ctx); err == nil {
		for _, t := range teams {
			s.Teams = append(s.Teams, *t)
		}
	}
	if items, err := rt.store.items(ctx, nil); err == nil {
		for _, it := range items {
			s.Items = append(s.Items, *it)
		}
	}
	if changes, err := rt.store.changes(ctx, nil); err == nil {
		for _, c := range changes {
			s.Changes = append(s.Changes, *c)
		}
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	s.LeaderSince = rt.leaderSince
	switch {
	case s.Installation == nil || len(s.Teams) == 0:
		// Nothing to decide yet: no pass is due.
		s.LastDecision = s.Now
	case rt.decidable.IsZero():
		// The Team is so new the leader hasn't seen it yet.
		s.LastDecision = s.Now
	default:
		s.LastDecision = rt.lastDecide
		if rt.decidable.After(s.LastDecision) {
			s.LastDecision = rt.decidable
		}
	}
	s.Verdicts = map[string]time.Time{
		v1alpha1.SourceWebhooks: rt.obs.webhooks.Verdict,
		v1alpha1.SourceBoard:    rt.obs.board.Verdict,
	}
	if len(rt.refusalRe) > 0 {
		s.RefusalRetries = map[string]int{}
		for k, v := range rt.refusalRe {
			s.RefusalRetries[k] = v
		}
	}
	return s
}

// syncDropped carries the history writer's dropped rows into engine_history_dropped_total.
func (rt *Runtime) syncDropped() {
	if rt.hist == nil {
		return
	}
	n := rt.hist.Dropped()
	rt.mu.Lock()
	delta := n - rt.dropped
	if delta > 0 {
		rt.dropped = n
	}
	rt.mu.Unlock()
	if delta > 0 {
		rt.m.HistoryDropped.Add(float64(delta))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
