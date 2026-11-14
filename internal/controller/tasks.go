package controller

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/decide"
	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/history"
	"github.com/giantswarm/beekeeper/internal/observe"
	"github.com/giantswarm/beekeeper/internal/wake"
)

// Observer tasks: requests of the observer controller, run one at a time.
const (
	taskStartup      = "startup"
	taskFilter       = "filter-check"
	taskRepos        = "installation-repos"
	taskBoardResync  = "board-resync"
	taskBoardProbe   = "board-probe"
	taskChangeResync = "change-resync"
	taskDeliveryLog  = "delivery-log"
	taskPoll         = "poll"
	taskResolve      = "resolve"
	taskClosing      = "closing-issues"
)

// Default intervals (the fixture's), used when the Installation leaves one unset.
const (
	defaultBoardResync  = 10 * time.Minute
	defaultChangeResync = 15 * time.Minute
	defaultDeliveryLog  = 5 * time.Minute
	defaultBoardProbe   = 10 * time.Minute
	defaultPolling      = 5 * time.Minute
	// probeFirst is a probe page: items without nested connections cost 1 point per 100.
	probeFirst   = 100
	resolveBatch = 100
	// closingBatch bounds the PRs whose closing issues one run reads (one small query each).
	closingBatch = 50
	// maxFailedPayloads bounds the failed deliveries whose payloads one delivery-log check reads; past
	// it the board and the Changes are resynced instead.
	maxFailedPayloads = 50
)

// observation is the leader's in-memory observation state.
type observation struct {
	webhooks, board *observe.Source
	restored        bool
	probes          map[string]*observe.Probe
	projectID       string
	filterChecked   bool
	unknown         []string
	reposETag       string
	lastBoardResync time.Time
	// taskSource is the source a task's next run attributes what it observes to.
	taskSource map[string]string
	// periodic marks a task run by its periodic timer (which then schedules the next run).
	periodic  map[string]bool
	taskRetry map[string]retryState
	// taskDeliveries are the deliveries waiting for a task's next successful run.
	taskDeliveries map[string][]string
	// parked are scheduled reads GitHub refused (by operation/team), with the inputs they were refused
	// on: they aren't made again until the installation's repos or the configuration change.
	parked map[string]string
	// resolve are board item nodes to read, and closing the PRs whose closing issues to read, with what
	// asked for them.
	resolve map[string]*taskReq
	closing map[string]*taskReq
	// flagged are board item nodes the probe found changed without a webhook, by team option: the next
	// resync reads those it doesn't return.
	flagged map[string]map[string]bool

	// The start-up sequence holds team passes until every Item and Change it touched was reconciled.
	starting     bool
	startupQueue map[string]bool
	heldTeams    map[string]string
	startupRan   bool
}

func newObservation() *observation {
	return &observation{
		webhooks: observe.NewSource(v1alpha1.SourceWebhooks, nil, time.Time{}),
		board:    observe.NewSource(v1alpha1.SourceBoard, nil, time.Time{}),
		probes:   map[string]*observe.Probe{}, taskSource: map[string]string{}, periodic: map[string]bool{},
		taskRetry: map[string]retryState{}, taskDeliveries: map[string][]string{}, parked: map[string]string{},
		resolve: map[string]*taskReq{}, closing: map[string]*taskReq{}, flagged: map[string]map[string]bool{},
		startupQueue: map[string]bool{}, heldTeams: map[string]string{},
	}
}

func dur(d metav1.Duration, def time.Duration) time.Duration {
	if d.Duration > 0 {
		return d.Duration
	}
	return def
}

// installation is the namespace's Installation (the first by name), nil when there is none.
func (rt *Runtime) installation(ctx context.Context) *v1alpha1.Installation {
	insts, err := rt.store.installations(ctx)
	if err != nil || len(insts) == 0 {
		return nil
	}
	return insts[0]
}

// runTask runs one observer task.
func (rt *Runtime) runTask(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	inst := rt.installation(ctx)
	if inst == nil {
		return reconcile.Result{}, nil
	}
	rt.applySpec(inst)
	task := req.Name
	rt.mu.Lock()
	source := rt.obs.taskSource[task]
	delete(rt.obs.taskSource, task)
	periodic := rt.obs.periodic[task]
	delete(rt.obs.periodic, task)
	deliveries := rt.obs.taskDeliveries[task]
	delete(rt.obs.taskDeliveries, task)
	rt.mu.Unlock()

	var err error
	switch task {
	case taskStartup:
		err = rt.startup(ctx, inst)
	case taskFilter:
		err = rt.checkFilter(ctx, inst)
	case taskRepos:
		err = rt.readRepos(ctx, orDefault(source, v1alpha1.SourceEngine))
	case taskBoardResync:
		if periodic {
			rt.scheduleBoardResync(inst, rt.clock.Now())
		}
		err = rt.boardResync(ctx, inst, orDefault(source, v1alpha1.SourceResync))
	case taskBoardProbe:
		if periodic {
			rt.schedulePeriodic(taskBoardProbe, rt.clock.Now().Add(dur(inst.Spec.Freshness.BoardProbe, defaultBoardProbe)), "")
		}
		err = rt.boardProbe(ctx, inst)
	case taskChangeResync:
		if periodic {
			rt.schedulePeriodic(taskChangeResync, rt.clock.Now().Add(dur(inst.Spec.Resync.Changes, defaultChangeResync)), v1alpha1.SourceResync)
		}
		err = rt.changeResync(ctx, orDefault(source, v1alpha1.SourceResync))
	case taskDeliveryLog:
		if periodic {
			rt.schedulePeriodic(taskDeliveryLog, rt.clock.Now().Add(dur(inst.Spec.Freshness.DeliveryLog, defaultDeliveryLog)), "")
		}
		err = rt.deliveryLog(ctx, inst)
	case taskPoll:
		rt.poll(ctx, inst, periodic)
	case taskResolve:
		err = rt.resolveNodes(ctx, inst)
	case taskClosing:
		err = rt.readClosing(ctx)
	}
	fe, isForge := forge.AsError(err)
	refused := isForge && fe.Kind == forge.Refused
	if err != nil && !refused && len(deliveries) > 0 {
		// The deliveries wait for the retry.
		rt.mu.Lock()
		rt.obs.taskDeliveries[task] = union(deliveries, rt.obs.taskDeliveries[task])
		rt.mu.Unlock()
	} else {
		// Done, or refused (the answer, not retried on these inputs).
		rt.unitsDone(deliveries, unitTask+task)
	}
	if err != nil {
		rt.retryTask(task, source, err, inst)
	} else {
		rt.mu.Lock()
		delete(rt.obs.taskRetry, task)
		rt.mu.Unlock()
	}
	rt.budgetChanged()
	return reconcile.Result{}, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// runTaskNow asks for a task with a source (merged by rank with one already asked for).
func (rt *Runtime) runTaskNow(task, source string) {
	rt.mu.Lock()
	if cur, ok := rt.obs.taskSource[task]; !ok || better(source, cur) {
		rt.obs.taskSource[task] = source
	}
	rt.mu.Unlock()
	rt.taskQ.Add(task)
}

func better(a, b string) bool {
	var p observe.Pending
	p.Merge(observe.Pending{Source: b})
	p.Merge(observe.Pending{Source: a})
	return p.Source == a && a != b
}

// schedulePeriodic runs task at at; its run schedules the next one.
func (rt *Runtime) schedulePeriodic(task string, at time.Time, source string) {
	rt.timers.Schedule("task/"+task, at, func() {
		rt.mu.Lock()
		rt.obs.periodic[task] = true
		if source != "" {
			if cur, ok := rt.obs.taskSource[task]; !ok || better(source, cur) {
				rt.obs.taskSource[task] = source
			}
		}
		rt.mu.Unlock()
		rt.taskQ.Add(task)
	})
}

// scheduleBoardResync schedules the next periodic board resync after one at now: every board resync
// interval, or every polling interval while the board source is degraded.
func (rt *Runtime) scheduleBoardResync(inst *v1alpha1.Installation, now time.Time) {
	rt.mu.Lock()
	polling := rt.obs.board.Polling
	rt.mu.Unlock()
	every := dur(inst.Spec.Resync.Board, defaultBoardResync)
	if polling {
		every = dur(inst.Spec.Polling.Interval, defaultPolling)
	}
	rt.schedulePeriodic(taskBoardResync, now.Add(every), v1alpha1.SourceResync)
}

// retryTask schedules a failed task again: at the reset of a rate limit or the reserve, after a
// secondary limit's Retry-After, or on the transient backoff (10s doubling to 5m). A retry is not a
// check of its own.
func (rt *Runtime) retryTask(task, source string, err error, inst *v1alpha1.Installation) {
	if fe, ok := forge.AsError(err); ok && fe.Kind == forge.Refused {
		// Refused: not retried on unchanged inputs (the read is parked; a configuration or installation
		// change, or the next periodic run on new inputs, makes it again).
		rt.log.Info("task refused, not retried until its inputs change", "task", task, "error", err.Error())
		rt.mu.Lock()
		delete(rt.obs.taskRetry, task)
		rt.mu.Unlock()
		return
	}
	now := rt.clock.Now()
	rt.mu.Lock()
	rs := rt.obs.taskRetry[task]
	at := retryAt(err, now, rs.attempt, inst)
	rs.attempt++
	rs.at = at
	rt.obs.taskRetry[task] = rs
	rt.mu.Unlock()
	rt.log.Info("task failed, retrying", "task", task, "at", at, "error", err.Error())
	rt.timers.Schedule("retry/"+task, at, func() { rt.runTaskNow(task, source) })
}

// retryAt is when a failed read is made again.
func retryAt(err error, now time.Time, attempt int, inst *v1alpha1.Installation) time.Time {
	initial, capd := 10*time.Second, 5*time.Minute
	if inst != nil {
		initial = dur(inst.Spec.Retry.Initial, initial)
		capd = dur(inst.Spec.Retry.Cap, capd)
	}
	at := now.Add(observe.Backoff(initial, capd, attempt))
	if fe, ok := forge.AsError(err); ok {
		switch fe.Kind {
		case forge.RateLimited, forge.Deferred:
			if fe.ResetAt.After(now) {
				return fe.ResetAt
			}
		case forge.SecondaryRateLimited:
			if fe.RetryAfter > 0 {
				return now.Add(fe.RetryAfter)
			}
			if fe.ResetAt.After(now) {
				return fe.ResetAt
			}
		}
	}
	return at
}

// startup is the leader's start-up sequence: restore the sources, read the installation's repos,
// check the board filter, resync the board and every Change, make the first freshness checks, rebuild
// the wait timers and schedule the periodic tasks. Team passes wait until every Item and Change it
// touched was reconciled.
func (rt *Runtime) startup(ctx context.Context, inst *v1alpha1.Installation) error {
	rt.mu.Lock()
	if rt.obs.startupRan {
		rt.mu.Unlock()
		return nil
	}
	rt.obs.startupRan = true
	rt.obs.starting = true
	now := rt.clock.Now()
	rt.obs.webhooks = observe.NewSource(v1alpha1.SourceWebhooks, sourceStatus(inst, v1alpha1.SourceWebhooks), now)
	rt.obs.board = observe.NewSource(v1alpha1.SourceBoard, sourceStatus(inst, v1alpha1.SourceBoard), now)
	rt.obs.restored = true
	rt.mu.Unlock()
	rt.inputs.SetOrg(inst.Spec.Org)
	rt.lastConfigVersion(ctx, inst)

	if err := rt.readRepos(ctx, v1alpha1.SourceStartup); err != nil {
		rt.log.Error(err, "read the installation's repos")
		rt.retryTask(taskRepos, v1alpha1.SourceEngine, err, inst)
	}
	if err := rt.checkFilter(ctx, inst); err != nil {
		rt.retryTask(taskFilter, "", err, inst)
	}
	// The Changes there were before start-up (a restart) are read again; those the board resync's Items
	// create are read once, when they are created.
	existing, err := rt.store.changes(ctx, nil)
	if err != nil {
		rt.log.Error(err, "list changes")
	}
	if err := rt.boardResync(ctx, inst, v1alpha1.SourceStartup); err != nil {
		rt.retryTask(taskBoardResync, v1alpha1.SourceStartup, err, inst)
	}
	for _, c := range existing {
		rt.addPending(KindChange, c.Name, observe.Pending{Trigger: decide.TriggerStartup, Source: v1alpha1.SourceStartup,
			Reads: observe.FullReads()})
	}
	if err := rt.deliveryLog(ctx, inst); err != nil {
		rt.retryTask(taskDeliveryLog, "", err, inst)
	}
	if err := rt.boardProbe(ctx, inst); err != nil {
		rt.retryTask(taskBoardProbe, "", err, inst)
	}

	// Rebuild the wait timers from the objects: a deadline that passed while no replica led is handled
	// now, once.
	now = rt.clock.Now()
	if items, err := rt.store.items(ctx, nil); err == nil {
		for _, it := range items {
			rt.rescheduleWait(KindItem, it.Name, it.Status.Wait, now)
		}
	}
	if changes, err := rt.store.changes(ctx, nil); err == nil {
		for _, c := range changes {
			rt.rescheduleWait(KindChange, c.Name, c.Status.Wait, now)
		}
	}

	rt.scheduleBoardResync(inst, now)
	rt.schedulePeriodic(taskBoardProbe, now.Add(dur(inst.Spec.Freshness.BoardProbe, defaultBoardProbe)), "")
	rt.schedulePeriodic(taskChangeResync, now.Add(dur(inst.Spec.Resync.Changes, defaultChangeResync)), v1alpha1.SourceResync)
	rt.schedulePeriodic(taskDeliveryLog, now.Add(dur(inst.Spec.Freshness.DeliveryLog, defaultDeliveryLog)), "")
	rt.mu.Lock()
	polling := rt.obs.webhooks.Polling
	rt.mu.Unlock()
	if polling {
		rt.schedulePeriodic(taskPoll, now.Add(dur(inst.Spec.Polling.Interval, defaultPolling)), v1alpha1.SourceReobserve)
	}

	if teams, err := rt.store.teams(ctx); err == nil {
		for _, t := range teams {
			rt.teamPass(t.Name, v1alpha1.SourceStartup)
		}
		rt.sawTeams(len(teams) > 0)
	}
	rt.mu.Lock()
	rt.obs.starting = false
	startedUp := rt.startedUp
	rt.mu.Unlock()
	rt.startupProgress("")
	rt.instQ.Add(instKey)
	select {
	case <-startedUp:
	default:
		close(startedUp)
	}
	return nil
}

func sourceStatus(inst *v1alpha1.Installation, name string) *v1alpha1.SourceStatus {
	if st, ok := inst.Status.Sources[name]; ok {
		return &st
	}
	return nil
}

// startupProgress records that key's reconcile ran; once the start-up sequence is done and nothing it
// touched is left, the held team passes run.
func (rt *Runtime) startupProgress(key string) {
	rt.mu.Lock()
	if key != "" {
		delete(rt.obs.startupQueue, key)
	}
	if rt.obs.starting || len(rt.obs.startupQueue) > 0 || len(rt.obs.heldTeams) == 0 {
		rt.mu.Unlock()
		return
	}
	held := rt.obs.heldTeams
	rt.obs.heldTeams = map[string]string{}
	rt.mu.Unlock()
	for _, team := range sortedKeys(held) {
		rt.teamPass(team, held[team])
	}
}

// touch notes that key will be reconciled as part of start-up.
func (rt *Runtime) touch(key string) {
	rt.mu.Lock()
	if rt.obs.starting || len(rt.obs.startupQueue) > 0 {
		rt.obs.startupQueue[key] = true
	}
	rt.mu.Unlock()
}

// teamPass asks for team's decision pass, attributed to source. During start-up it waits until every
// Item was decided.
func (rt *Runtime) teamPass(team, source string) {
	if team == "" {
		return
	}
	rt.mu.Lock()
	if rt.obs.starting || len(rt.obs.startupQueue) > 0 {
		if cur, ok := rt.obs.heldTeams[team]; !ok || better(source, cur) {
			rt.obs.heldTeams[team] = source
		}
		rt.mu.Unlock()
		return
	}
	if cur, ok := rt.teamSource[team]; !ok || better(source, cur) {
		rt.teamSource[team] = source
	}
	rt.mu.Unlock()
	rt.teamQ.Add(team)
}

// readRepos reads the installation's repos; when they changed, refused reads are tried again.
func (rt *Runtime) readRepos(ctx context.Context, source string) error {
	rt.mu.Lock()
	etag := rt.obs.reposETag
	rt.mu.Unlock()
	repos, meta, err := rt.forge.InstallationRepos(forge.WithClass(ctx, forge.ClassObservation), etag)
	if err != nil {
		return err
	}
	if meta.NotModified {
		return nil
	}
	rt.mu.Lock()
	rt.obs.reposETag = meta.ETag
	rt.mu.Unlock()
	rt.inputs.SetRepos(repos)
	rt.rules.Forget()
	if source != v1alpha1.SourceStartup {
		rt.retryRefused(ctx, source)
	}
	return nil
}

// retryRefused reads every object whose read stands refused again (its inputs changed).
func (rt *Runtime) retryRefused(ctx context.Context, source string) {
	trigger := triggerOf(source)
	if items, err := rt.store.items(ctx, func(it *v1alpha1.Item) bool { return it.Status.Refusal != nil }); err == nil {
		for _, it := range items {
			rt.addPending(KindItem, it.Name, observe.Pending{Trigger: trigger, Source: source, ReadBoard: true})
		}
	}
	if changes, err := rt.store.changes(ctx, func(c *v1alpha1.Change) bool { return c.Status.Refusal != nil }); err == nil {
		for _, c := range changes {
			rt.addPending(KindChange, c.Name, observe.Pending{Trigger: trigger, Source: source, Reads: observe.FullReads()})
		}
	}
}

// addPending records what an object's next reconcile observes and asks for it; the deliveries that
// asked for it wait for that reconcile.
func (rt *Runtime) addPending(kind, name string, p observe.Pending) {
	key := kind + "/" + name
	for _, id := range p.Deliveries {
		rt.holdUnit(id, key)
	}
	rt.pending.Add(key, p)
	rt.touch(key)
	rt.enqueue(kind, name)
}

func triggerOf(source string) decide.Trigger {
	switch source {
	case v1alpha1.SourceWebhook:
		return decide.TriggerWebhook
	case v1alpha1.SourceResync:
		return decide.TriggerResync
	case v1alpha1.SourceStartup:
		return decide.TriggerStartup
	case v1alpha1.SourceReobserve:
		return decide.TriggerPoll
	}
	return decide.TriggerEngine
}

// checkFilter checks the teams' board options against the board schema (BoardFilterValid). An unknown
// option degrades the board source (filter-invalid) and keeps that team's board queries from running;
// a configuration that makes it valid again runs a board resync at once.
func (rt *Runtime) checkFilter(ctx context.Context, inst *v1alpha1.Installation) error {
	if rt.parkedOn(forge.OpBoardSchema) {
		return nil
	}
	schema, err := rt.forge.BoardSchema(forge.WithClass(ctx, forge.ClassObservation), inst.Spec.Org, inst.Spec.Board.Project)
	if err != nil {
		rt.park(forge.OpBoardSchema, err)
		rt.boardCheckFailed(err)
		return err
	}
	teams, err := rt.store.teams(ctx)
	if err != nil {
		return err
	}
	var options []string
	for _, t := range teams {
		options = append(options, t.Spec.BoardOption)
	}
	unknown := observe.UnknownOptions(schema, options)
	now := rt.clock.Now()
	rt.mu.Lock()
	wasInvalid := rt.obs.filterChecked && len(rt.obs.unknown) > 0
	rt.obs.filterChecked, rt.obs.unknown, rt.obs.projectID = true, unknown, schema.ProjectID
	var tr observe.Transition
	if len(unknown) > 0 {
		tr = rt.obs.board.Gap(now, v1alpha1.SourceReasonFilterInvalid)
	} else if rt.obs.board.Reason == v1alpha1.SourceReasonFilterInvalid {
		tr = rt.obs.board.Resolve(now)
	}
	rt.mu.Unlock()
	rt.sourceTransition(v1alpha1.SourceBoard, tr)
	rt.instQ.Add(instKey)
	if wasInvalid && len(unknown) == 0 {
		rt.runTaskNow(taskBoardResync, v1alpha1.SourceResync)
	}
	return nil
}

// validTeams are the Teams whose board option the filter check accepted.
func (rt *Runtime) validTeams(ctx context.Context) []*v1alpha1.Team {
	teams, err := rt.store.teams(ctx)
	if err != nil {
		return nil
	}
	rt.mu.Lock()
	unknown := map[string]bool{}
	for _, o := range rt.obs.unknown {
		unknown[o] = true
	}
	rt.mu.Unlock()
	var out []*v1alpha1.Team
	for _, t := range teams {
		if t.Spec.BoardOption != "" && !unknown[t.Spec.BoardOption] {
			out = append(out, t)
		}
	}
	return out
}

// graphQLPaused reports until when GraphQL reads aren't made (a primary rate limit or the reserve).
func (rt *Runtime) graphQLPaused() (time.Time, bool) {
	b := rt.forge.Budget().GraphQL
	now := rt.clock.Now()
	if b.Paused && b.PausedUntil.After(now) {
		return b.PausedUntil, true
	}
	return time.Time{}, false
}

// graphQLFailed degrades the board source when a GraphQL read a webhook asked for is deferred by the
// budget (the board can't be observed); other failures of such reads are retried and degrade nothing.
func (rt *Runtime) graphQLFailed(err error) {
	fe, ok := forge.AsError(err)
	if !ok || (fe.Kind != forge.RateLimited && fe.Kind != forge.Deferred) {
		return
	}
	rt.boardGap(v1alpha1.SourceReasonBudget)
}

// boardCheckFailed degrades the board source at a failed scheduled check (a resync, the probe, the
// filter check): budget when the budget deferred it, else board-unreadable (an error or a refusal). A
// secondary rate limit is a pause, not a failed check: it is retried after Retry-After.
func (rt *Runtime) boardCheckFailed(err error) {
	reason := v1alpha1.SourceReasonBoardUnreadable
	if fe, ok := forge.AsError(err); ok {
		switch fe.Kind {
		case forge.SecondaryRateLimited:
			return
		case forge.RateLimited, forge.Deferred:
			reason = v1alpha1.SourceReasonBudget
		}
	}
	rt.boardGap(reason)
}

func (rt *Runtime) boardGap(reason string) {
	now := rt.clock.Now()
	rt.mu.Lock()
	tr := rt.obs.board.Gap(now, reason)
	rt.mu.Unlock()
	rt.sourceTransition(v1alpha1.SourceBoard, tr)
	rt.instQ.Add(instKey)
}

// refusalInputs are what a refused scheduled read depends on: the installation's repos and the
// configuration.
func (rt *Runtime) refusalInputs() string { return rt.inputs.Key("", "") }

// park records a scheduled read GitHub refused (op names the read, with its team), with a refusal row:
// it isn't made again until its inputs change. It reports whether err was a refusal.
func (rt *Runtime) park(op string, err error) bool {
	fe, ok := forge.AsError(err)
	if !ok || fe.Kind != forge.Refused {
		return false
	}
	inputs := rt.refusalInputs()
	rt.mu.Lock()
	rt.obs.parked[op] = inputs
	rt.mu.Unlock()
	rt.append(history.Event{At: rt.clock.Now(), Type: observe.EventRefusal, Source: v1alpha1.SourceEngine,
		Data: map[string]any{"operation": fe.Operation, "code": fe.Code}})
	return true
}

// parkedOn reports whether the scheduled read op stands refused on the current inputs.
func (rt *Runtime) parkedOn(op string) bool {
	inputs := rt.refusalInputs()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	at, ok := rt.obs.parked[op]
	if ok && at == inputs && !rt.cfg.Faults.ForgetRefusals.Load() {
		return true
	}
	delete(rt.obs.parked, op)
	return false
}

// anyParked reports whether a scheduled board read of a valid team stands refused: the board can't be
// observed, whatever the probe says.
func (rt *Runtime) anyParked(teams []*v1alpha1.Team) bool {
	if rt.parkedOn(forge.OpBoardSchema) {
		return true
	}
	for _, t := range teams {
		if rt.parkedOn(forge.OpBoardTeamItems+"/"+t.Spec.BoardOption) || rt.parkedOn(forge.OpBoardProbe+"/"+t.Spec.BoardOption) {
			return true
		}
	}
	return false
}

func pausedError(until time.Time) error {
	return &forge.Error{Kind: forge.Deferred, API: forge.APIGraphQL, ResetAt: until}
}

// boardResync runs the filtered board query of every valid team: it creates Items for new items and
// hands changed ones to their Items. The Items the query doesn't return are read by node id in batches
// when that can tell something: those it should have returned (to learn why it didn't), the Merged and
// Validating ones whose issue the merge closed (tracked until the board says Done), and the ones the
// probe found changed. Then each team's decision pass runs. A team whose query stands refused is
// skipped until the inputs change.
func (rt *Runtime) boardResync(ctx context.Context, inst *v1alpha1.Installation, source string) error {
	if until, paused := rt.graphQLPaused(); paused {
		rt.boardCheckFailed(pausedError(until))
		return pausedError(until)
	}
	trigger := triggerOf(source)
	req := taskReq{Source: source, Trigger: trigger}
	for _, team := range rt.validTeams(ctx) {
		op := forge.OpBoardTeamItems + "/" + team.Spec.BoardOption
		if rt.parkedOn(op) {
			// The team is still decided on what was observed (the board source shows why it is stale).
			rt.teamPass(team.Name, source)
			continue
		}
		res, err := rt.forge.BoardTeamItems(forge.WithClass(ctx, forge.ClassObservation),
			observe.TeamQuery(inst.Spec.Org, inst.Spec.Board.Project, team.Spec.BoardOption))
		if err != nil {
			rt.boardCheckFailed(err)
			rt.teamPass(team.Name, source)
			if rt.park(op, err) {
				continue
			}
			return err
		}
		returned := map[string]bool{}
		for _, list := range [][]forge.BoardItem{res.Active, res.InboxBugs} {
			for i := range list {
				bi := list[i]
				if !bi.IsIssue || bi.Issue.Ref == "" {
					continue
				}
				name := v1alpha1.ItemName(bi.Issue.Ref)
				returned[name] = true
				if err := rt.offerBoardItem(ctx, team, name, bi, req); err != nil {
					return err
				}
			}
		}
		items, err := rt.store.items(ctx, func(it *v1alpha1.Item) bool { return it.Labels[v1alpha1.LabelTeam] == team.Name })
		if err != nil {
			return err
		}
		rt.mu.Lock()
		flagged := rt.obs.flagged[team.Spec.BoardOption]
		delete(rt.obs.flagged, team.Spec.BoardOption)
		rt.mu.Unlock()
		byNode := map[string]*v1alpha1.Item{}
		for _, it := range items {
			if returned[it.Name] {
				continue
			}
			st := it.Status
			if st.Issue == nil || st.Board == nil || st.Board.NodeID == "" {
				rt.addPending(KindItem, it.Name, req.pending(observe.Pending{ReadBoard: true}))
				continue
			}
			if observe.InQuery(st.Board.Status, st.Board.Kind, st.Issue.State) || st.Phase == v1alpha1.PhaseMerged ||
				st.Phase == v1alpha1.PhaseValidating || flagged[st.Board.NodeID] {
				byNode[st.Board.NodeID] = it
			}
		}
		if err := rt.readItems(ctx, team, byNode, req); err != nil {
			rt.boardCheckFailed(err)
			return err
		}
		rt.teamPass(team.Name, source)
	}
	rt.mu.Lock()
	rt.obs.lastBoardResync = rt.clock.Now()
	rt.mu.Unlock()
	rt.prune(ctx)
	return nil
}

// readItems reads tracked Items' board items by node id, resolveBatch per request, and hands each to
// its Item (one the board no longer has leaves by scope).
func (rt *Runtime) readItems(ctx context.Context, team *v1alpha1.Team, byNode map[string]*v1alpha1.Item, req taskReq) error {
	nodes := sortedKeys(byNode)
	for len(nodes) > 0 {
		batch := nodes[:min(len(nodes), resolveBatch)]
		nodes = nodes[len(batch):]
		got, err := rt.forge.BoardItemsByNode(forge.WithClass(ctx, forge.ClassObservation), batch)
		if err != nil {
			return err
		}
		for _, node := range batch {
			it := byNode[node]
			bi, ok := got[node]
			if !ok {
				rt.addPending(KindItem, it.Name, req.pending(observe.Pending{Board: &forge.BoardItem{}, Found: false}))
				continue
			}
			if err := rt.offerBoardItem(ctx, team, it.Name, bi, req); err != nil {
				return err
			}
		}
	}
	return nil
}

// offerBoardItem gives a board item the team tracks to its Item name, creating the Item when there is
// none. An Item whose facts the item doesn't change isn't reconciled. An Item that can't be created is
// skipped (and counted as a reconcile error), so one item never stops a resync.
func (rt *Runtime) offerBoardItem(ctx context.Context, team *v1alpha1.Team, name string, bi forge.BoardItem, req taskReq) error {
	key := KindItem + "/" + name
	var it v1alpha1.Item
	err := rt.store.get(ctx, KindItem, name, &it)
	switch {
	case apierrors.IsNotFound(err):
		if !observe.Tracks(bi, team.Spec.BoardOption) {
			return nil
		}
		p := req.pending(observe.Pending{Board: &bi, Found: true})
		for _, id := range p.Deliveries {
			rt.holdUnit(id, key)
		}
		rt.pending.Add(key, p)
		rt.touch(key)
		obj := &v1alpha1.Item{ObjectMeta: metav1.ObjectMeta{Name: name,
			Labels:      map[string]string{v1alpha1.LabelTeam: team.Name},
			Annotations: map[string]string{AnnotationRef: bi.Issue.Ref}}}
		if err := rt.store.create(ctx, KindItem, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			rt.log.Error(err, "create an Item; skipped until the next resync", "item", bi.Issue.Ref)
			rt.m.ReconcileErrors.WithLabelValues(KindItem).Inc()
			rt.dropPending(key)
			rt.startupProgress(key)
			return nil
		}
		rt.itemQ.Add(name)
		return nil
	case err != nil:
		return err
	}
	cur := it.Status.DeepCopy()
	observe.ApplyBoardItem(cur, bi)
	if it.Status.Issue != nil && len(observe.DiffItem(&it.Status, cur)) == 0 &&
		observe.LeftScope(bi, true, team.Spec.BoardOption) == "" && req.Source != v1alpha1.SourceStartup {
		return nil
	}
	rt.addPending(KindItem, name, req.pending(observe.Pending{Board: &bi, Found: true}))
	return nil
}

// prune drops what the leader keeps in memory about objects that are gone: the would-actions recorded
// (keyed by item ref), the refusal retries, and the webhook counts no standing refusal depends on.
func (rt *Runtime) prune(ctx context.Context) {
	items, err := rt.store.items(ctx, nil)
	if err != nil {
		return
	}
	changes, err := rt.store.changes(ctx, nil)
	if err != nil {
		return
	}
	refs, watched := map[string]bool{}, map[string]bool{}
	note := func(ref string, refused bool) {
		refs[ref] = true
		if refused {
			watched[ref], watched[refRepo(ref)] = true, true
		}
	}
	for _, it := range items {
		note(itemRef(it), it.Status.Refusal != nil)
	}
	for _, c := range changes {
		note(changeRef(c), c.Status.Refusal != nil)
	}
	rt.inputs.Retain(func(k string) bool { return watched[k] })
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for k := range rt.actions {
		if !refs[actionRef(k)] {
			delete(rt.actions, k)
		}
	}
	for ref := range rt.refusalRe {
		if !refs[ref] {
			delete(rt.refusalRe, ref)
		}
	}
}

// actionRef is the item ref of a would-action key (owner/repo#n/type/who).
func actionRef(key string) string {
	hash := strings.IndexByte(key, '#')
	if hash < 0 {
		return key
	}
	if slash := strings.IndexByte(key[hash:], '/'); slash >= 0 {
		return key[:hash+slash]
	}
	return key
}

// changeResync reads every Change again, conditionally (an unchanged answer is a free 304).
func (rt *Runtime) changeResync(ctx context.Context, source string) error {
	changes, err := rt.store.changes(ctx, nil)
	if err != nil {
		return err
	}
	for _, c := range changes {
		rt.addPending(KindChange, c.Name, observe.Pending{Trigger: triggerOf(source), Source: source, Reads: observe.FullReads()})
	}
	return nil
}

// deliveryLog reads the App's webhook delivery log since the last check. A failed delivery degrades
// the webhooks source and its payload names what to re-observe; a log that can't be read degrades it
// too (delivery-log-unreadable). It never redelivers.
func (rt *Runtime) deliveryLog(ctx context.Context, inst *v1alpha1.Installation) error {
	if rt.cfg.Faults.StopFreshness.Load() {
		return nil
	}
	now := rt.clock.Now()
	interval := dur(inst.Spec.Freshness.DeliveryLog, defaultDeliveryLog)
	rt.mu.Lock()
	cursor := rt.obs.webhooks.Cursor
	rt.mu.Unlock()
	deliveries, next, err := rt.forge.Deliveries(forge.WithClass(ctx, forge.ClassObservation), cursor)
	if err != nil {
		if fe, ok := forge.AsError(err); ok && fe.Kind == forge.SecondaryRateLimited {
			return err // a pause, not a check: retried after Retry-After
		}
		reason := v1alpha1.SourceReasonLogUnreadable
		if fe, ok := forge.AsError(err); ok && (fe.Kind == forge.RateLimited || fe.Kind == forge.Deferred) {
			reason = v1alpha1.SourceReasonBudget
		}
		rt.mu.Lock()
		tr := rt.obs.webhooks.Gap(now, reason)
		rt.mu.Unlock()
		rt.webhooksTransition(inst, tr, now)
		rt.instQ.Add(instKey)
		return nil
	}
	rt.mu.Lock()
	failed, tr := rt.obs.webhooks.LogCheck(now, interval, deliveries)
	if next != "" {
		rt.obs.webhooks.Cursor = next
	}
	rt.mu.Unlock()
	// Only deliveries that name something to read are worth their payload; past a bound, resyncing is
	// cheaper than one GET per delivery.
	var relevant []forge.Delivery
	for _, d := range failed {
		if wake.Subscribed[d.Event] && (d.Event != "projects_v2_item" || d.Action != "reordered") {
			relevant = append(relevant, d)
		}
	}
	if len(relevant) > maxFailedPayloads {
		rt.log.Info("many failed deliveries: resyncing instead of reading their payloads", "failed", len(relevant))
		rt.runTaskNow(taskBoardResync, v1alpha1.SourceReobserve)
		rt.runTaskNow(taskChangeResync, v1alpha1.SourceReobserve)
		rt.runTaskNow(taskRepos, v1alpha1.SourceReobserve)
		relevant = nil
	}
	for _, d := range relevant {
		detail, err := rt.forge.Delivery(forge.WithClass(ctx, forge.ClassObservation), d.ID)
		if err != nil {
			rt.log.Error(err, "read a failed delivery", "id", d.ID)
			continue
		}
		event := detail.Event
		if event == "" {
			event = d.Event
		}
		m, err := wake.Map(event, detail.Payload)
		if err != nil {
			rt.log.Error(err, "map a failed delivery", "id", d.ID)
			continue
		}
		rt.route(ctx, m, v1alpha1.SourceReobserve, decide.TriggerPoll, "")
	}
	rt.webhooksTransition(inst, tr, now)
	rt.instQ.Add(instKey)
	return nil
}

// webhooksTransition records a change of the webhooks source and starts or stops polling.
func (rt *Runtime) webhooksTransition(inst *v1alpha1.Installation, tr observe.Transition, now time.Time) {
	rt.sourceTransition(v1alpha1.SourceWebhooks, tr)
	switch tr {
	case observe.Degraded:
		rt.schedulePeriodic(taskPoll, now.Add(dur(inst.Spec.Polling.Interval, defaultPolling)), v1alpha1.SourceReobserve)
	case observe.Fresh:
		rt.timers.Cancel("task/" + taskPoll)
	}
}

// sourceTransition records source.degraded and source.fresh rows.
func (rt *Runtime) sourceTransition(source string, tr observe.Transition) {
	if tr == observe.Unchanged {
		return
	}
	rt.mu.Lock()
	src := rt.obs.webhooks
	if source == v1alpha1.SourceBoard {
		src = rt.obs.board
	}
	typ, reason := "source.fresh", ""
	if tr == observe.Degraded {
		typ, reason = "source.degraded", src.Reason
	}
	rt.mu.Unlock()
	rt.append(history.Event{At: rt.clock.Now(), Type: typ, Source: v1alpha1.SourceEngine,
		Data: map[string]any{"source": source, "reason": reason}})
}

// boardProbe asks every valid team's board items updated since its last probe (newest first) whether
// a board change came without a webhook. It only judges freshness; the resync observes, and reads the
// items it flagged that its query doesn't return. A team whose probe stands refused is skipped until the
// inputs change, and the board stays degraded meanwhile.
func (rt *Runtime) boardProbe(ctx context.Context, inst *v1alpha1.Installation) error {
	if rt.cfg.Faults.StopFreshness.Load() {
		return nil
	}
	now := rt.clock.Now()
	rt.mu.Lock()
	invalid := len(rt.obs.unknown) > 0
	rt.mu.Unlock()
	if until, paused := rt.graphQLPaused(); paused {
		rt.boardCheckFailed(pausedError(until))
		return pausedError(until)
	}
	gap := false
	teams := rt.validTeams(ctx)
	for _, team := range teams {
		option := team.Spec.BoardOption
		op := forge.OpBoardProbe + "/" + option
		if rt.parkedOn(op) {
			continue
		}
		rt.mu.Lock()
		pr := rt.obs.probes[option]
		if pr == nil {
			pr = &observe.Probe{}
			rt.obs.probes[option] = pr
		}
		since := pr.Since()
		rt.mu.Unlock()
		p, err := rt.forge.BoardProbe(forge.WithClass(ctx, forge.ClassObservation), forge.ProbeQuery{Org: inst.Spec.Org,
			Project: inst.Spec.Board.Project, TeamOption: option, First: probeFirst, UpdatedSince: since})
		if err != nil {
			rt.boardCheckFailed(err)
			if rt.park(op, err) {
				continue
			}
			return err
		}
		rt.mu.Lock()
		if gaps := pr.Judge(p.Items); len(gaps) > 0 || p.Truncated {
			// More items changed than the probe reads: what it didn't read can't be vouched for.
			gap = true
			if rt.obs.flagged[option] == nil {
				rt.obs.flagged[option] = map[string]bool{}
			}
			for _, g := range gaps {
				rt.obs.flagged[option][g.NodeID] = true
			}
		}
		rt.mu.Unlock()
	}
	unreadable := rt.anyParked(teams)
	interval := dur(inst.Spec.Freshness.BoardProbe, defaultBoardProbe)
	rt.mu.Lock()
	var tr observe.Transition
	switch {
	case invalid:
		tr = rt.obs.board.Gap(now, v1alpha1.SourceReasonFilterInvalid)
	case unreadable:
		tr = rt.obs.board.Gap(now, v1alpha1.SourceReasonBoardUnreadable)
	case gap:
		tr = rt.obs.board.Gap(now, v1alpha1.SourceReasonProbeGap)
	default:
		tr = rt.obs.board.Clean(now, interval, false)
	}
	last := rt.obs.lastBoardResync
	rt.mu.Unlock()
	rt.sourceTransition(v1alpha1.SourceBoard, tr)
	switch tr {
	case observe.Degraded:
		// Poll the board: the next resync one polling interval from now, unless one is due earlier.
		at := now.Add(dur(inst.Spec.Polling.Interval, defaultPolling))
		if cur, ok := rt.timers.At("task/" + taskBoardResync); !ok || at.Before(cur) {
			rt.schedulePeriodic(taskBoardResync, at, v1alpha1.SourceResync)
		}
	case observe.Fresh:
		at := last.Add(dur(inst.Spec.Resync.Board, defaultBoardResync))
		if !at.After(now) {
			at = now
		}
		rt.schedulePeriodic(taskBoardResync, at, v1alpha1.SourceResync)
	}
	rt.instQ.Add(instKey)
	return nil
}

// poll re-observes every wait while the webhooks source is degraded, conditionally where possible.
func (rt *Runtime) poll(ctx context.Context, inst *v1alpha1.Installation, periodic bool) {
	rt.mu.Lock()
	degraded := rt.obs.webhooks.Degraded()
	rt.mu.Unlock()
	if !degraded {
		rt.timers.Cancel("task/" + taskPoll)
		return
	}
	now := rt.clock.Now()
	if periodic {
		rt.schedulePeriodic(taskPoll, now.Add(dur(inst.Spec.Polling.Interval, defaultPolling)), v1alpha1.SourceReobserve)
	}
	if items, err := rt.store.items(ctx, func(it *v1alpha1.Item) bool { return it.Status.Wait != nil }); err == nil {
		for _, it := range items {
			rt.addPending(KindItem, it.Name, observe.Pending{Trigger: decide.TriggerPoll, Source: v1alpha1.SourceReobserve, ReadBoard: true})
		}
	}
	if changes, err := rt.store.changes(ctx, func(c *v1alpha1.Change) bool { return c.Status.Wait != nil }); err == nil {
		for _, c := range changes {
			rt.addPending(KindChange, c.Name, observe.Pending{Trigger: decide.TriggerPoll, Source: v1alpha1.SourceReobserve,
				Reads: observe.WaitReads(c.Status.Wait)})
		}
	}
}

// resolveNodes reads the board items webhooks named that no Item owns, in one request, and creates an
// Item for each one a team tracks. Others are dropped.
func (rt *Runtime) resolveNodes(ctx context.Context, _ *v1alpha1.Installation) error {
	nodes, more := rt.takeRequests(&rt.obs.resolve, resolveBatch)
	if len(nodes) == 0 {
		return nil
	}
	if more {
		rt.taskQ.Add(taskResolve)
	}
	if until, paused := rt.graphQLPaused(); paused {
		rt.putBack(&rt.obs.resolve, nodes)
		rt.graphQLFailed(pausedError(until))
		return pausedError(until)
	}
	ids := sortedKeys(nodes)
	got, err := rt.forge.BoardItemsByNode(forge.WithClass(ctx, forge.ClassObservation), ids)
	if err != nil {
		if fe, ok := forge.AsError(err); ok && fe.Kind == forge.Refused {
			// Nothing to learn from these nodes: the resync is the safety net.
			rt.log.Info("board items refused", "nodes", ids, "error", err.Error())
			for _, id := range ids {
				rt.unitsDone(nodes[id].Deliveries, unitResolve+id)
			}
			return nil
		}
		rt.putBack(&rt.obs.resolve, nodes)
		rt.graphQLFailed(err)
		return err
	}
	teams := rt.validTeams(ctx)
	for _, id := range ids {
		req := *nodes[id]
		if bi, ok := got[id]; ok {
			if req.Source == v1alpha1.SourceWebhook {
				rt.reported(bi)
			}
			for _, t := range teams {
				if t.Spec.BoardOption == bi.Team && bi.IsIssue && bi.Issue.Ref != "" {
					if err := rt.offerBoardItem(ctx, t, v1alpha1.ItemName(bi.Issue.Ref), bi, req); err != nil {
						return err
					}
				}
			}
		}
		rt.unitsDone(req.Deliveries, unitResolve+id)
	}
	return nil
}

// takeRequests takes up to n requests (in key order) from one of the observer's request maps, and
// reports whether more are left.
func (rt *Runtime) takeRequests(m *map[string]*taskReq, n int) (map[string]*taskReq, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	keys := sortedKeys(*m)
	if len(keys) > n {
		keys = keys[:n]
	}
	out := map[string]*taskReq{}
	for _, k := range keys {
		out[k] = (*m)[k]
		delete(*m, k)
	}
	return out, len(*m) > 0
}

// putBack returns requests whose reads failed, to be retried.
func (rt *Runtime) putBack(m *map[string]*taskReq, reqs map[string]*taskReq) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for k, r := range reqs {
		if cur, ok := (*m)[k]; ok {
			cur.merge(*r)
			continue
		}
		(*m)[k] = r
	}
}

// readClosing reads the issues the PRs webhooks named close, and re-reads the Items they close and the
// Items that listed them, attributed to what asked (a pull_request webhook vouches for the link only).
// One small GraphQL query per PR; never a board resync.
func (rt *Runtime) readClosing(ctx context.Context) error {
	prs, more := rt.takeRequests(&rt.obs.closing, closingBatch)
	if len(prs) == 0 {
		return nil
	}
	if more {
		rt.taskQ.Add(taskClosing)
	}
	if until, paused := rt.graphQLPaused(); paused {
		rt.putBack(&rt.obs.closing, prs)
		rt.graphQLFailed(pausedError(until))
		return pausedError(until)
	}
	keys := sortedKeys(prs)
	for i, pr := range keys {
		req := *prs[pr]
		linked, err := rt.forge.PullClosingIssues(readCtx(ctx), pr)
		if err != nil {
			if fe, ok := forge.AsError(err); ok && fe.Kind == forge.Refused {
				rt.log.Info("closing issues refused", "pr", pr, "error", err.Error())
				rt.unitsDone(req.Deliveries, unitClosing+pr)
				continue
			}
			left := map[string]*taskReq{}
			for _, k := range keys[i:] {
				left[k] = prs[k]
			}
			rt.putBack(&rt.obs.closing, left)
			rt.graphQLFailed(err)
			return err
		}
		names := map[string]bool{}
		for _, l := range linked {
			names[v1alpha1.ItemName(l.Ref)] = true
		}
		listed, err := rt.store.items(ctx, func(it *v1alpha1.Item) bool { return containsStr(it.Status.Changes, pr) },
			client.MatchingFields{wake.IndexChanges: pr})
		if err == nil {
			for _, it := range listed {
				names[it.Name] = true
			}
		}
		for _, name := range sortedKeys(names) {
			var it v1alpha1.Item
			if err := rt.store.get(ctx, KindItem, name, &it); err != nil {
				continue // not tracked: the PR closes something no team tracks
			}
			rt.addPending(KindItem, name, req.pending(observe.Pending{ReadBoard: true}))
		}
		rt.unitsDone(req.Deliveries, unitClosing+pr)
	}
	return nil
}

// reported records a board item update a webhook announced, for the probe.
func (rt *Runtime) reported(bi forge.BoardItem) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	pr := rt.obs.probes[bi.Team]
	if pr == nil {
		pr = &observe.Probe{}
		rt.obs.probes[bi.Team] = pr
	}
	pr.Reported(bi.NodeID, bi.UpdatedAt)
}

// lastConfigVersion returns the configuration's version (the generations of the Installation, the
// Teams and the People), and records it as the refusal inputs' configuration.
func (rt *Runtime) lastConfigVersion(ctx context.Context, inst *v1alpha1.Installation) string {
	v := []string{"inst=" + strconv.FormatInt(inst.Generation, 10)}
	if teams, err := rt.store.teams(ctx); err == nil {
		for _, t := range teams {
			v = append(v, "team/"+t.Name+"="+strconv.FormatInt(t.Generation, 10))
		}
	}
	if people, err := rt.store.people(ctx); err == nil {
		for _, p := range people {
			v = append(v, "person/"+p.Name+"="+strconv.FormatInt(p.Generation, 10))
		}
	}
	sort.Strings(v)
	s := ""
	for _, x := range v {
		s += x + ";"
	}
	rt.inputs.SetConfig(s)
	return s
}
