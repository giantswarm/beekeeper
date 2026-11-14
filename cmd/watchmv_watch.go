package cmd

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/claude"
	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/notify"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/sink"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
	"github.com/giantswarm/beekeeper/internal/upgrade"
)

type watcher struct {
	a *app
	// batchEvery is immutable once the loops start; batch is guarded by mu.
	batchEvery time.Duration
	batch      []string
	// loopOwned leaves event actions to the deterministic supervisor.
	loopOwned bool
	mu        sync.Mutex
	last      map[string]time.Time
	// active are the lasting conditions said and not yet ENDED.
	active map[string]condition
	// markFile keeps active and reported for a restarted watch; dirty is
	// set when they changed since it was written.
	markFile string
	dirty    bool
	// feedID is the id of the last central feed event the watch printed,
	// kept in its mark.
	feedID          string
	lastPoll        time.Time
	suspendEntry    time.Time
	suspendChecked  time.Time
	suspendThrough  time.Time
	suspendRecovery map[string]suspendWake
	suspendDeliver  func(context.Context, suspendWake) error
	// Hung checks get a fresh grace period on startup and after a suspend
	// (or a long gap in observations). These times stay in memory so their
	// monotonic readings can exclude time spent asleep.
	lastHungCheck time.Time
	hungGrace     time.Time
	// refusedSince is the newest hook.refused event said; zero before the
	// first reading, which said none.
	refusedSince time.Time
	lastBudget   time.Time
	// lastKindSpend is when the day's spend per task kind was last summed,
	// kindSumming set while a sum runs.
	lastKindSpend time.Time
	kindSumming   atomic.Bool
	scopeOOM      int64
	sessions      map[string]*claude.Session
	seenKills     map[string]bool
	// reported are the stale leases said once.
	reported map[string]bool
	// notifier sends the events that need a person (--notify); nil prints only.
	notifier *notify.Notifier
	// swapSamples are watch.swapWindow's swap readings, oldest first: the
	// growth rate toward systemd-oomd's trigger.
	swapSamples []swapSample
	// swap is the machine sample's latest swap reading, under mu: the
	// poll's CAPACITY lines weigh a start against it.
	swap *capacity.SwapReading
	// readHeadroom reads the headroom of a start; nil is app.readHeadroom.
	readHeadroom func(context.Context, *capacity.SwapReading) *capacity.Headroom
	// standby leaves a running supervisor's events to its watch.
	standby bool
	// guideRecovery owns crash recovery on its own clock in a continuous watch.
	guideRecovery bool
	// upgrades are the last upgrade readings of the installations.
	upgrades        upgrade.Readings
	upgradeFailures map[string]int
	// noContextSaid are the readers (ALERTS, UPGRADES) whose installations
	// without a kube context this watch said.
	noContextSaid map[string]bool
	// records are the session records of the last poll: their sessions'
	// ends get the record's line instead of the SESSIONS ended one.
	records []state.Record
	// stopped are the agents with a task this watch said have no running
	// CLI, by session key, until their CLI runs again.
	stopped map[string]bool
	// rowlessSaid are the workers this watch said have no row in the
	// desktop, by session, until they get one.
	rowlessSaid map[string]bool
	// waits are the reopen waits this watch said, by agent session and the
	// wait's start, until they end.
	waits map[string]time.Time
	// stand is the standby watch's memory of its messages, successors and
	// reopens; table the last poll's process table.
	stand standbyWatch
	table *proc.Table
	// feedbackNext is when the feedback watch reads the report threads
	// next; feedbackBusy is set while a read runs.
	feedbackNext time.Time
	feedbackBusy atomic.Bool
	// deliver sends a reply to an agent; nil is agents wake's.
	deliver func(ctx context.Context, q, msg string) error
	// musterRun runs muster for the feedback watch; nil is the binary.
	musterRun central.Runner
	// runReport launches a reporter's turn; nil is launchReport.
	runReport func(unit, id, name, prompt string) error
	// turnEnded reports whether a reporter's unit ended; nil is unitEnded.
	turnEnded func(context.Context, string) bool
	// readHRs reads a lane installation's HelmReleases; nil is kubectl.
	readHRs func(context.Context, config.Lane) ([]merge.HelmRelease, error)
	// clock paces the loops and dates the polls; nil is the wall clock.
	clock watchClock
	// cpuTable is the process table the last machine sample read, at
	// cpuAt: the next one's top CPU consumers are measured against it.
	// cpuOver counts the samples in a row with CPU pressure over
	// watch.cpuPSIMax.
	cpuTable *proc.Table
	cpuAt    time.Time
	cpuOver  int
	// forks is the fork counter the last machine sample read; forkUsual
	// the machine's usual fork rate, a slow average of the rates outside a
	// storm; forkOver counts the samples in a row more than
	// watch.forkRateMax over it (PROCESS STORM).
	forks     uint64
	forkUsual float64
	forkOver  int
	// disk is the free space on / over the last minutes and the processes'
	// writes, for DISK FILLING.
	disk diskWatch
	// readForks reads the fork counter; nil is plat.Machine.Forks.
	readForks func() (uint64, error)
	// self is the watch's PID in the process table; 0 is its own.
	self int
	// owners names the session of each CLI PID the last poll found: the
	// machine sample attributes a storm or a stack with it.
	owners atomic.Pointer[map[int]string]
	// strained is the machine sample's verdict that the CPU is saturated:
	// the installation reads slow down (READS SLOWED).
	strained atomic.Bool
	// settling, budgeting and usageBusy are set while a lane read, a budget
	// probe or a subscription usage read runs: the next poll skips it instead
	// of queueing a second one.
	settling, budgeting, usageBusy atomic.Bool
	lastSettle                     time.Time
	lastUsage                      time.Time
	usageErr                       string
	usageReported                  time.Time
	providerLastUsage              map[string]time.Time
	providerUsageErr               map[string]string
	executionSyncFailures          map[string]int
	executionInventorySynced       map[string]int
	// sinkPending are the events of the sink's verbs within sink.Window,
	// sinking is set while their dispatch runs, sinkLedger claims each one
	// once across the machine's watches.
	sinkPending []state.Event
	sinking     atomic.Bool
	sinkLedger  *sink.Ledger
	// sweeping is set while the exposure sweep runs, lastSweep when it began.
	sweeping  atomic.Bool
	lastSweep time.Time
	// serving is set while the self-hosted providers' serving modes are
	// reconciled, lastServing when that last began: a drain waits for the
	// model's sessions, and the watch runs the switch the moment they are
	// gone.
	serving     atomic.Bool
	lastServing time.Time
	// teleporting is set while the Teleport login is read, lastTeleport
	// when it began.
	teleporting     atomic.Bool
	lastTeleport    time.Time
	lastClaudeLogin string
	// centraling is set while the roster is published to the central
	// instance, lastCentral when that last started and centralChecked when
	// the central instance was last called.
	centraling     atomic.Bool
	lastCentral    time.Time
	centralChecked time.Time
	// polls counts the polls begun.
	polls atomic.Int64
	// failed counts the reads that failed (fail): --once exits non-zero on
	// any, so a silent run means no change.
	failed        atomic.Int64
	resolverState resolverWatch
	// missing are the sections whose platform part this build does not
	// have, said once each.
	missing map[string]bool
	// chores runs the doctor every poll (not for --once); doctoring is set
	// while its run goes, and retitled holds when it last retitled each
	// desktop session.
	chores    bool
	doctoring atomic.Bool
	retitled  map[string]time.Time
	// goCacheOn trims the Go build cache (not for --once); goCaching is
	// set while a look at it goes, goCacheNext is when the next is due.
	goCacheOn   bool
	goCaching   atomic.Bool
	goCacheNext time.Time
	// dockerOn prunes Docker's images and build cache (not for --once);
	// dockerPruning is set while a look at it goes, dockerNext is when the
	// next is due.
	dockerOn      bool
	dockerPruning atomic.Bool
	dockerNext    time.Time
	// freeOn runs the automatic free of tmpfs (not for --once); freeing is
	// set while a run goes, freeNext is when the next is due and lastFree
	// the last run any watch did, for the TMPFS line.
	freeOn   bool
	freeing  atomic.Bool
	freeNext time.Time
	lastFree atomic.Pointer[freeMark]
	// diskPruneOn enables the scheduled and pressure-triggered disk step.
	// diskPruning orders access to diskPruneNext across background steps.
	diskPruneOn   bool
	diskPruning   atomic.Bool
	diskPruneNext time.Time
	// holdersOn lists the state home's holders (not for --once);
	// holdering orders access to holdersNext across background looks.
	holdersOn   bool
	holdering   atomic.Bool
	holdersNext time.Time
	// worktreesOn trims finished workers' worktrees for the legacy ask mode.
	worktreesOn       bool
	worktreesTrimming atomic.Bool
	worktreesNext     time.Time
	// slotsOn stops orphaned slot scopes (not for --once); orphanSince is
	// when the watch first saw each orphaned slot scope.
	slotsOn          bool
	orphanSince      map[string]time.Time
	emptyHolderSince map[string]time.Time
	// doctored are the chore lines the doctor said, each said once.
	doctored map[string]bool
	// timerActs are the fired timers' wakes and commands under way, and
	// the defaulted decisions' filers being told.
	timerActs sync.WaitGroup
	// bg are the poll's background runs under way (inFlight, the doctor,
	// the Go cache's, Docker's and tmpfs's looks): a restart waits for them.
	bg sync.WaitGroup
	// restart tells the watch when an install asked it to restart on the
	// installed binary (nil: never, --once); restartFor is the request
	// its run ended for.
	restart    *restartWatch
	restartFor *restartRequest
	// openNotes are the notes open at the last tick, answersSince the time
	// up to which their answers were said.
	openNotes    map[int]bool
	answersSince time.Time
	// replaced says whether a process's binary was replaced, and its path;
	// nil is platform.ProcessBinary. versionOf is the version a binary file
	// reports; nil runs it. gateOf runs budget --gate on an installed binary.
	replaced    func(pid int) (string, bool)
	versionOf   func(ctx context.Context, file string) string
	gateOf      func(ctx context.Context, file string) error
	gateChecked map[string]bool
}

// unavailable reports whether err is a platform part this build does not
// have. The first time per section the watch says so in one line, never
// as a condition or an alert.
func (w *watcher) unavailable(section string, err error) bool {
	if !platform.Missing(err) {
		return false
	}
	w.mu.Lock()
	said := w.missing[section]
	if w.missing == nil {
		w.missing = map[string]bool{}
	}
	w.missing[section] = true
	w.mu.Unlock()
	if !said {
		w.emitNow("unavailable", "%s", platform.Unavailable(section))
	}
	return true
}

// helmReleases reads the lane installation's HelmReleases.
func (w *watcher) helmReleases(ctx context.Context, lane config.Lane) ([]merge.HelmRelease, error) {
	if w.readHRs != nil {
		return w.readHRs(ctx, lane)
	}
	return gate.ReadHelmReleases(ctx, lane)
}

// watchClock is a watch's time: its loops wait on its timers.
type watchClock interface {
	Now() time.Time
	// Timer fires once after d; stop releases it.
	Timer(d time.Duration) (fire <-chan time.Time, stop func())
}

// wallClock is the time a watch keeps outside its tests: beekeeper's clock,
// the system's in a release build and a scenario world's in a scenario.
type wallClock struct{}

func (wallClock) Now() time.Time { return clk.Now() }

func (wallClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	t := clk.NewTimer(d)
	return t.C, func() { t.Stop() }
}

func (w *watcher) clk() watchClock {
	if w.clock != nil {
		return w.clock
	}
	return wallClock{}
}

func (w *watcher) run(ctx context.Context, once bool) error {
	if once {
		w.batchEvery = 0
	} else if w.batchEvery == 0 {
		w.batchEvery = w.a.cfg.Watch.BatchInterval.Duration
	}
	w.lastPoll = w.clk().Now().Add(-w.a.cfg.Watch.Interval.Duration)
	s, err := plat.Machine.DesktopScope()
	if s != nil {
		w.scopeOOM = s.OOMKills
	}
	if !w.unavailable(secScope, err) {
		w.check("noscope", s == nil, "no Claude Desktop scope found; watching the machine numbers only")
	}
	interval := w.a.cfg.Watch.Interval.Duration
	if once {
		w.a.ctx = ctx
		w.upgradeCycle(ctx)
		w.sample(ctx)
		w.lastSweep = clk.Now() // swept here, not in the poll's background
		w.exposures()
		w.lastKindSpend = w.clk().Now() // summed here, not in the poll's background
		w.kindSpending(w.lastKindSpend)
		w.poll(ctx)
		w.stand.inflight.Wait() // a successor's start outlives no watch
		w.timerActs.Wait()      // nor a timer's wake or command
		return w.onceErr()
	}
	w.diskPruneOn = !w.loopOwned
	w.goCacheOn = true
	w.holdersOn = true
	w.dockerOn = true
	w.freeOn = true
	w.worktreesOn = true
	w.slotsOn = true
	w.readFreeMark()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	w.a.ctx = ctx
	var wg sync.WaitGroup
	if w.standby {
		w.guideRecovery = true
		g := w.newGuideRecovery()
		wg.Go(func() {
			defer g.stand.inflight.Wait()
			g.loop(ctx, min(interval, 30*time.Second), false, g.recoverGuide)
		})
	}
	// The machine is sampled in a loop of its own, so no network read or
	// subprocess of the rest of the poll ever delays a memory or load line.
	// The first sample comes before the reads start: they begin knowing
	// whether the machine is strained. The loop's own first sample is one
	// interval later: two samples in a row span one (CPU PRESSURE).
	w.sample(ctx)
	defer wg.Wait()
	if w.batchEvery > 0 {
		wg.Go(func() { w.batchLoop(ctx) })
	}
	wg.Go(func() {
		fire, stop := w.clk().Timer(interval)
		defer stop()
		select {
		case <-ctx.Done():
		case <-fire:
			w.loop(ctx, interval, false, w.sample)
		}
	})
	if !w.standby {
		wg.Go(func() { w.watchAlerts(ctx) })
		if w.a.cfg.Alerts.PagerDuty.Enabled() {
			wg.Go(func() { w.watchPagerDuty(ctx) })
		}
		if w.a.cfg.Alerts.Discover.Enabled() {
			wg.Go(func() { w.watchDiscovery(ctx) })
		}
	}
	if w.a.cfg.Central.Enabled() {
		w.a.hub() // made before the feed, the mailbox and the roster sync share it
		wg.Go(func() { w.watchFeed(ctx) })
		if w.standby {
			wg.Go(func() { w.watchMailbox(ctx) })
		}
	}
	wg.Go(func() {
		w.loop(ctx, interval, true, func(ctx context.Context) {
			if w.loopOwned {
				if _, replaced := w.replacedBinary(os.Getpid()); replaced {
					return
				}
			}
			w.upgradeCycle(ctx)
		})
	})
	w.loop(ctx, interval, false, func(ctx context.Context) {
		if w.loopOwned {
			if _, replaced := w.replacedBinary(os.Getpid()); replaced {
				return
			}
		}
		w.poll(ctx)
		if req := w.restartDue(); req != nil {
			w.checkInstalledGate(ctx, req.Binary)
			w.restartFor = req
			cancel(errRestart)
		}
	})
	// Every shutdown waits for the runs the watch started, including saves
	// still writing its state after cancellation.
	wg.Wait()
	w.bg.Wait()
	w.stand.inflight.Wait()
	w.timerActs.Wait()
	if w.restartFor == nil {
		return nil
	}
	// A restarted watch's mark says nothing twice.
	w.saveMark()
	return errRestart
}

// restartDue is the install's request this watch restarts for, nil when
// none.
func (w *watcher) restartDue() *restartRequest {
	if w.restart == nil {
		return nil
	}
	return w.restart.due()
}

// slowReads is how many times less often the installation reads run while
// the machine is strained.
const slowReads = 4

// loop runs fn every interval until ctx ends. A run that overruns skips the
// ticks it missed instead of queueing them. An installation read (reads)
// runs every slowReads × interval, at nice 10, while the machine is
// strained.
func (w *watcher) loop(ctx context.Context, interval time.Duration, reads bool, fn func(context.Context)) {
	own := w.clk()
	for {
		start, every, rctx := own.Now(), interval, ctx
		if reads {
			every, rctx = w.readEvery(interval), w.readCtx(ctx)
		}
		fn(rctx)
		next := start.Add(every)
		now := own.Now()
		for !next.After(now) {
			next = next.Add(every)
		}
		fire, stop := own.Timer(next.Sub(now))
		select {
		case <-ctx.Done():
			stop()
			return
		case <-fire:
		}
	}
}

// readEvery is an installation read's interval: slowReads times longer while
// the machine is strained.
func (w *watcher) readEvery(interval time.Duration) time.Duration {
	if w.strained.Load() {
		return slowReads * interval
	}
	return interval
}

// readCtx runs an installation read's commands at nice 10 while the machine
// is strained.
func (w *watcher) readCtx(ctx context.Context) context.Context {
	if w.strained.Load() {
		return proc.Background(ctx)
	}
	return ctx
}

// inFlight runs fn unless its previous run (running) still goes, and waits
// for it at most wait: a hung read holds the poll up for one wait at most,
// and the next poll skips it instead of starting a second one. fn keeps
// running on ctx, within its own timeouts.
func (w *watcher) inFlight(ctx context.Context, wait time.Duration, running *atomic.Bool, fn func(context.Context)) {
	if ctx.Err() != nil || !running.CompareAndSwap(false, true) {
		return
	}
	done := make(chan struct{})
	w.bg.Add(1)
	go func() {
		defer w.bg.Done()
		defer running.Store(false)
		defer close(done)
		fn(ctx)
	}()
	t := clk.NewTimer(wait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	case <-ctx.Done():
	}
}

// watchAlerts reads the alerts every alerts.every while this watch owns the
// baseline; another watch that owns it is said once, and this one takes over
// when it ends.
func (w *watcher) watchAlerts(ctx context.Context) {
	store := alerts.NewStore(w.a.cfg.StateDir)
	defer func() { _ = store.Release() }()
	// One port-forward per installation, kept from tick to tick: a tick
	// is then HTTP requests, not kubectl and tsh processes.
	reader := w.a.alertReader()
	reader.Keep = true
	defer reader.Close()
	other := 0
	w.loop(ctx, w.a.cfg.Alerts.Every.Duration, true, func(ctx context.Context) {
		owned, owner, err := store.Own()
		switch {
		case err != nil:
			w.emit("alerts", "ALERTS baseline unusable: %v", err)
		case !owned:
			if base, err := store.Load(); err == nil {
				w.publishAlerts(base.Firing(""))
			}
			reader.Close()
			w.clear("alerts")
			if owner.PID != other {
				other = owner.PID
				w.emitNow("alerts", "ALERTS read by the watch with pid %d; this one takes over when it ends", other)
			}
		default:
			w.clear("alerts")
			if other != 0 {
				other = 0
				w.emitNow("alerts", "ALERTS taken over by this watch")
			}
			lines, noContext := w.a.alertCycle(ctx, store, reader)
			if base, err := store.Load(); err == nil {
				w.publishAlerts(base.Firing(""))
			}
			for _, l := range lines {
				w.emitNow("alerts", "%s", l)
			}
			w.sayNoContext("ALERTS", noContext)
		}
	})
}

// emit says a lasting condition once, when it starts. While it lasts it is
// silent and returns its line at most every watch.repeat, for the
// notification a person gets (notify.repeat decides whether it goes out).
func (w *watcher) emit(key, format string, args ...any) string {
	line := fmt.Sprintf(format, args...)
	w.mu.Lock()
	now := clk.Now()
	if w.active == nil {
		w.active = map[string]condition{}
	}
	_, active := w.active[key]
	if active && now.Sub(w.last[key]) < w.a.cfg.Watch.Repeat.Duration {
		w.mu.Unlock()
		return ""
	}
	w.last[key] = now
	if !active {
		label, _, _ := strings.Cut(line, ":")
		w.active[key] = condition{Since: now, Label: label}
		w.dirty = true
	}
	w.mu.Unlock()
	if !active {
		w.emitNow(key, "%s", line)
	}
	return line
}

// onceErr is --once's outcome: an error once a read failed, its lines said
// already.
func (w *watcher) onceErr() error {
	if n := w.failed.Load(); n > 0 {
		return fmt.Errorf("watch --once: %d reads failed, said above", n)
	}
	return nil
}

// fail says a read that failed as a condition (emit) and counts it: --once
// exits non-zero on any.
func (w *watcher) fail(key, format string, args ...any) string {
	w.failed.Add(1)
	return w.emit(key, format, args...)
}

// check says a condition's start (emit) while on holds and its end, one
// ENDED line, once it no longer does.
func (w *watcher) check(key string, on bool, format string, args ...any) string {
	if on {
		return w.emit(key, format, args...)
	}
	w.clear(key)
	return ""
}

// clear ends a condition the watch has said: one ENDED line.
func (w *watcher) clear(key string) {
	w.mu.Lock()
	c, active := w.active[key]
	delete(w.active, key)
	w.dirty = w.dirty || active
	w.mu.Unlock()
	if active {
		if w.a.store != nil {
			_ = w.a.store.Record(event(watchParty, "watch.clear", "%s", key))
		}
		w.emitNow("ended", "ENDED %s (since %s)", c.Label, c.Since.Local().Format("15:04"))
	}
}

// isActive reports whether the watch has said the condition key and not yet
// its end.
func (w *watcher) isActive(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, active := w.active[key]
	return active
}

// clearMissing ends each condition said under prefix that found no longer
// holds: one ENDED line each.
func (w *watcher) clearMissing(prefix string, found map[string]bool) {
	w.mu.Lock()
	var gone []string
	for k := range w.active {
		if strings.HasPrefix(k, prefix) && !found[k] {
			gone = append(gone, k)
		}
	}
	w.mu.Unlock()
	for _, k := range gone {
		w.clear(k)
	}
}

// drop forgets each condition said under prefix that found no longer holds,
// without an ENDED line, and answers them.
func (w *watcher) drop(prefix string, found map[string]bool) map[string]condition {
	w.mu.Lock()
	defer w.mu.Unlock()
	gone := map[string]condition{}
	for k, c := range w.active {
		if strings.HasPrefix(k, prefix) && !found[k] {
			gone[k] = c
			delete(w.active, k)
			w.dirty = true
		}
	}
	return gone
}

// condition is a lasting condition a watch has said: since when, and the
// line's head that names it.
type condition struct {
	Since time.Time `json:"since"`
	Label string    `json:"label"`
}

// watchMark is what a caller's watch has said and a restarted watch of the
// same caller must not say again: the conditions open (it says only their
// end) and the one-time events (runaways, stale leases).
type watchMark struct {
	Conditions    map[string]condition `json:"conditions"`
	Reported      map[string]bool      `json:"reported"`
	FeedID        string               `json:"feedID,omitempty"`
	UsageReported time.Time            `json:"usageReported,omitzero"`
}

// saveMark keeps what the watch has said for its successor, when it changed.
func (w *watcher) saveMark() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.markFile == "" || !w.dirty {
		return
	}
	w.dirty = false
	_ = w.a.store.WriteFile(w.markFile, watchMark{Conditions: w.active, Reported: w.reported, FeedID: w.feedID, UsageReported: w.usageReported})
}

// notify sends one event that needs a person (--notify): a lasting kind
// takes no key.
func (w *watcher) notify(ctx context.Context, kind, key, summary, body string) {
	w.notifyAt(ctx, w.a.now, kind, key, summary, body)
}

// notifyAt is notify for a loop of its own, which keeps its own time.
func (w *watcher) notifyAt(ctx context.Context, now time.Time, kind, key, summary, body string) {
	if w.notifier != nil && body != "" {
		w.notifier.Notify(ctx, now, kind, key, summary, body)
	}
}

// swapSample is one poll's swap, zswap's writeback since boot and
// MemAvailable.
type swapSample struct {
	at                                         time.Time
	usedMiB, diskMiB, writtenBackMiB, availMiB int
}

// swapTrend is the swap's movement over watch.swapWindow: disk swap's
// growth from new swap-outs, zswap's writeback to disk, the growth of all
// swap in use (what systemd-oomd measures), and whether MemAvailable fell.
// Rated once the readings span config.MinSwapSpan.
type swapTrend struct {
	// DiskPerHourMiB is disk swap's growth less zswap's writeback: pages
	// zswap moves to disk leave swap in use flat and are no new swapping.
	DiskPerHourMiB int
	// WritebackPerHourMiB is zswap's writeback to the swap device.
	WritebackPerHourMiB int
	UsedPerHourMiB      int
	AvailFalling        bool
	Rated               bool
	// Window is how far back the rate looks.
	Window time.Duration
}

// pressing reports disk swap growing while MemAvailable falls: the only
// swap reading that means memory pressure. Swapped pages stay in swap long
// after the burst that pushed them out, and zswap's share sits in RAM.
func (t swapTrend) pressing() bool { return t.Rated && t.DiskPerHourMiB > 0 && t.AvailFalling }

// swapTrend records a reading and returns the movement over
// watch.swapWindow.
func (w *watcher) swapTrend(now time.Time, m machine.Mem) swapTrend {
	window := w.a.cfg.Watch.SwapWindow.Duration
	w.swapSamples = append(w.swapSamples, swapSample{now, m.SwapUsedMiB, m.DiskSwapMiB(), m.ZswapWrittenBackMiB, m.AvailableMiB})
	i := 0
	for i < len(w.swapSamples)-1 && now.Sub(w.swapSamples[i].at) > window {
		i++
	}
	w.swapSamples = w.swapSamples[i:]
	first := w.swapSamples[0]
	span := now.Sub(first.at)
	if span < config.MinSwapSpan {
		return swapTrend{Window: window}
	}
	perHour := func(mib int) int { return int(float64(mib) / span.Hours()) }
	writtenBack := max(0, m.ZswapWrittenBackMiB-first.writtenBackMiB)
	return swapTrend{
		DiskPerHourMiB:      perHour(m.DiskSwapMiB() - first.diskMiB - writtenBack),
		WritebackPerHourMiB: perHour(writtenBack),
		UsedPerHourMiB:      perHour(m.SwapUsedMiB - first.usedMiB),
		AvailFalling:        m.AvailableMiB < first.availMiB,
		Rated:               true,
		Window:              window,
	}
}

// swapLine says swap in use as disk plus zswap, systemd-oomd's swap rule
// (its trigger only when it watches a cgroup for swap), disk swap's growth
// from new swap-outs and zswap's writeback apart: the numbers that decide
// whether swap means pressure.
func swapLine(m machine.Mem, oomd *machine.OOMDSwap, t swapTrend) string {
	line := fmt.Sprintf("SWAP: %d of %d MiB used, %s", m.SwapUsedMiB, m.SwapTotalMiB, m.SwapSplit())
	if oomd == nil {
		line += ", systemd-oomd swap rule unknown"
	} else {
		line += ", " + oomd.Line(m)
	}
	if !t.Rated {
		return line + ", growth not yet measured"
	}
	line += fmt.Sprintf(", disk %+d MiB/h, zswap writeback %d MiB/h over the last %s", t.DiskPerHourMiB, t.WritebackPerHourMiB, dur(t.Window))
	if t.pressing() {
		line += " while MemAvailable falls"
	}
	if oomd != nil && oomd.Watched() && t.UsedPerHourMiB > 0 && oomd.HeadroomMiB(m) > 0 {
		line += ", trigger in " + untilTrigger(oomd.HeadroomMiB(m), t.UsedPerHourMiB).Round(time.Minute).String()
	}
	return line
}

// swapOver reports disk swap over maxMiB and pressing; zswap's share,
// held in RAM, never counts.
func swapOver(m machine.Mem, t swapTrend, maxMiB int) bool {
	return m.DiskSwapMiB() > maxMiB && t.pressing()
}

// untilTrigger is how long headroom lasts at perHour.
func untilTrigger(headroom, perHour int) time.Duration {
	return time.Duration(float64(headroom) / float64(perHour) * float64(time.Hour))
}

// oomdImminent reports whether systemd-oomd's swap kill is near enough to
// need a person: oomd watches a cgroup for swap, disk swap grows while
// MemAvailable falls, and the headroom is under watch.oomdHeadroomMinMiB
// (a fraction of SwapTotal by default) or the trigger within
// watch.oomdWithin at the growth of the swap in use.
func (w *watcher) oomdImminent(m machine.Mem, oomd *machine.OOMDSwap, t swapTrend) bool {
	if oomd == nil || !oomd.Watched() || !t.pressing() {
		return false
	}
	th := w.a.cfg.Watch
	headroom := oomd.HeadroomMiB(m)
	if headroom < th.OOMDHeadroomMin(m.SwapTotalMiB) {
		return true
	}
	return t.UsedPerHourMiB > 0 && untilTrigger(headroom, t.UsedPerHourMiB) < th.OOMDWithin.Duration
}

// modelServer says the host models no model-server lease covers and
// unloads those a lab loaded or a budget exceeds, unless ollama.nameOnly.
func (w *watcher) modelServer(ctx context.Context, models []machine.HostModel) {
	holders, err := lease.Dir(w.a.cfg.LeaseDir).List()
	if err != nil {
		return
	}
	bs := modelBreaches(models, holders, w.a.cfg.Resources, w.a.cfg.Ollama.BudgetGiB)
	unloaded := map[string]error{}
	for _, b := range bs {
		if b.Unload && !w.a.cfg.Ollama.NameOnly {
			unloaded[b.Model.Name] = b.Model.Unload(ctx, serverURL(w.a.cfg, b.Model))
		}
	}
	w.check("modelserver", len(bs) > 0, "MODEL SERVER: %s", modelServerLine(bs, unloaded))
}

// emitNow records every event immediately, then batches only known routine lines.
func (w *watcher) emitNow(key string, format string, args ...any) {
	w.emitLinesNow(key, []string{fmt.Sprintf(format, args...)})
}

func (w *watcher) emitLinesNow(key string, lines []string) {
	var evs []state.Event
	var kept []string
	for _, line := range lines {
		if key != "resolver" && w.resolverFailure(line) {
			continue
		}
		kept = append(kept, line)
		evs = append(evs, event(watchParty, "watch."+key, "%s", line))
	}
	if w.a.store != nil && len(evs) > 0 {
		_ = w.a.store.Record(evs...)
	}
	for _, line := range kept {
		w.outputEvent(line)
	}
}

// sample reads the machine's numbers (memory, swap, load, pressure, disk,
// the desktop scope) from /proc and /sys, and decides whether the machine
// is strained. It keeps its own time: it runs in a loop of its own.
func (w *watcher) sample(ctx context.Context) {
	now := clk.Now()
	if st, err := w.a.readSnapshot(); err == nil {
		w.sayAppDrifts(w.a.appDrifts(ctx, st.Apps))
	}
	th := w.a.cfg.Watch

	// GTT is RAM the iGPU pins outside every cgroup: above its threshold
	// it is named as the cause of low memory, with the model servers' models.
	models, merr := hostModels(ctx, w.a.cfg)
	if merr == nil {
		w.modelServer(ctx, models)
	}
	// One process table a sample: the swapoff and the CPU lines read it;
	// nil when it is unreadable.
	table, terr := plat.Machine.Processes()
	if terr != nil {
		table = nil
	}
	m, merr := plat.Machine.Mem()
	w.unavailable(secMemory, merr)
	var cause string
	if gpus := machine.ReadGPUs(); machine.GTTUsedMiB(gpus) > th.GTTMax(m.TotalMiB) {
		cause = gttLine(gpus, models)
	}
	w.check("gtt", cause != "", "IGPU %s", strings.TrimPrefix(cause, "iGPU "))
	if merr == nil {
		w.check("avail", m.AvailableMiB < th.AvailMin(m.TotalMiB), "LOW RAM: %d MiB available, swap %s%s", m.AvailableMiB, m.SwapSplit(), because(cause))
		var oomd *machine.OOMDSwap
		if o, err := plat.Machine.OOMDSwap(ctx); err == nil {
			oomd = &o
		}
		t := w.swapTrend(now, m)
		line := swapLine(m, oomd, t) + because(cause)
		w.check("swap", swapOver(m, t, th.SwapMax(m.SwapTotalMiB)), "%s", line)
		// A running swapoff shrinks SwapTotal ahead of the pages it drains:
		// swap reads full while it empties, and oomd is no nearer.
		swapoff := swapoffRuns(table)
		w.check("swapoff", swapoff, "SWAPOFF IN PROGRESS: %s", line)
		if m.SwapTotalMiB > 0 {
			w.check("oomd", !swapoff && w.oomdImminent(m, oomd, t), "OOMD IMMINENT: %s", line)
		}
		w.keepSwap(&capacity.SwapReading{At: now, UsedMiB: m.SwapUsedMiB, DiskMiB: m.DiskSwapMiB(), ZswapMiB: m.ZswappedMiB,
			PerHourMiB: t.DiskPerHourMiB, WritebackPerHourMiB: t.WritebackPerHourMiB, Rated: t.Rated})
	}
	w.sampleCPU(now, table)
	psi, err := plat.Machine.MemoryPressure()
	w.unavailable(secPressure, err)
	if err == nil {
		w.check("psi", psi > th.PSIMax, "MEMORY PRESSURE: full avg60 %.0f%%", psi)
	}
	if d, err := machine.ReadDisk("/tmp"); err == nil {
		large := d.UsedMiB > th.TmpMax(d.TotalMiB)
		var largest string
		if large {
			var owners map[int]string
			if o := w.owners.Load(); o != nil {
				owners = *o
			}
			largest = "; " + tmpfsLargest("/tmp", table, owners, proc.References)
		}
		w.check("tmp", large, "TMPFS /tmp: %d MiB; %s%s", d.UsedMiB, w.lastFreeNote(), largest)
	}
	if d, err := machine.ReadDisk("/"); err == nil {
		var owners map[int]string
		if o := w.owners.Load(); o != nil {
			owners = *o
		}
		w.sampleDisk(now, d, w.cpuTable, owners)
	}
	w.goCache(now)
	w.docker(now)
	w.autoFree(now)
	w.worktrees(now)
	w.diskPrune(now, false)
	w.stateHolders(now)
	w.slotOrphans(now, table)
	s, err := plat.Machine.DesktopScope()
	w.unavailable(secScope, err)
	if s != nil {
		w.check("scopeanon", s.AnonMiB > th.ScopeAnonMax(m.TotalMiB), "DESKTOP SCOPE anon: %d MiB", s.AnonMiB)
		if s.OOMKills != w.scopeOOM {
			w.emitNow("scopeoom", "OOM KILL in the desktop scope: oom_kill %d -> %d", w.scopeOOM, s.OOMKills)
			w.scopeOOM = s.OOMKills
		}
	}
	w.saveMark()
	if w.loopOwned {
		w.publishMachine()
	}
}

// sampleCPU says the CPU lines: HIGH LOAD over watch.loadMax (or
// watch.loadPerCoreMax × cores), LOAD RISING on a steep climb, CPU PRESSURE
// once two samples in a row read some avg10 over watch.cpuPSIMax, each
// with the top CPU consumers since the last sample; and READS SLOWED while
// the machine is strained.
func (w *watcher) sampleCPU(now time.Time, t *proc.Table) {
	th := w.a.cfg.Watch
	cores := runtime.NumCPU()
	limit := th.LoadLimit(cores)
	load, err := plat.Machine.Load()
	w.unavailable(secLoad, err)
	cpu, err := plat.Machine.CPUPressure()
	w.unavailable(secPressure, err)
	if cpu > th.CPUPSIMax {
		w.cpuOver++
	} else {
		w.cpuOver = 0
	}
	var top string
	if t != nil {
		span := now.Sub(w.cpuAt)
		top = topCPULine(topCPU(w.cpuTable, t, span, topCPUCommands), span)
		w.sampleProcs(now, span, w.cpuTable, t)
		w.cpuTable, w.cpuAt = t, now
	}
	w.check("load", load[0] > limit, "HIGH LOAD: 1m %.0f over %.0f (%d cores)%s", load[0], limit, cores, top)
	w.check("loadrising", loadRising(load, cores), "LOAD RISING: 1m %.0f, 5m %.0f (%d cores)%s", load[0], load[1], cores, top)
	// A restarted watch keeps a CPU PRESSURE it said while it lasts.
	said := w.isActive("cpupsi")
	w.check("cpupsi", w.cpuOver >= 2 || said && w.cpuOver > 0, "CPU PRESSURE: some avg10 %.0f%% over %.0f%%%s", cpu, th.CPUPSIMax, top)
	strained := load[0] > limit || cpu > th.CPUPSIMax
	w.strained.Store(strained)
	w.check("slowed", strained, "READS SLOWED: machine under CPU pressure (load %.0f, CPU some avg10 %.0f%%): installation reads every %d× their interval, at nice %s",
		load[0], cpu, slowReads, proc.Niceness)
}

// swapoffRuns reports whether a swapoff runs.
func swapoffRuns(t *proc.Table) bool {
	if t == nil {
		return false
	}
	for _, p := range t.ByPID {
		if p.Comm == "swapoff" {
			return true
		}
	}
	return false
}

// watchOwner names the watch's own processes in the PROCESS STORM, LOAD and
// STACKED lines.
const watchOwner = "beekeeper watch"

// sampleProcs says PROCESS STORM once two samples in a row read a fork rate
// more than watch.forkRateMax over the machine's usual one, LOAD while more
// than watch.toolProcsMax processes of watch.tools run, and a STACKED line
// for each command line running more than watch.stackMax times over, each
// with an ENDED line when it ends.
// prev is the process table the last sample read, span ago.
func (w *watcher) sampleProcs(now time.Time, span time.Duration, prev, t *proc.Table) {
	th := w.a.cfg.Watch
	owners := map[int]string{}
	if o := w.owners.Load(); o != nil {
		maps.Copy(owners, *o)
	}
	// The watch's own processes (its alert reads among them) are the
	// watch's, not the session's that started it.
	owners[cmp.Or(w.self, os.Getpid())] = watchOwner
	read := w.readForks
	if read == nil {
		read = plat.Machine.Forks
	}
	forks, err := read()
	if err == nil && w.forks > 0 && span > 0 && forks >= w.forks {
		rate := float64(forks-w.forks) / span.Seconds()
		if w.forkUsual == 0 {
			w.forkUsual = rate
		}
		if th.ForkRateMax > 0 && rate > w.forkUsual+th.ForkRateMax {
			w.forkOver++
		} else {
			w.forkOver = 0
			w.forkUsual += (rate - w.forkUsual) * min(1, span.Seconds()/forkUsualOver.Seconds())
		}
		// A restarted watch keeps a PROCESS STORM it said while it lasts.
		said := w.isActive("forks")
		if w.forkOver >= 2 || said && w.forkOver > 0 {
			w.emit("forks", "%s", stormLine(rate, w.forkUsual, fresh(prev, t), t, owners))
		} else {
			w.clear("forks")
		}
	}
	if err == nil {
		w.forks = forks
	}

	var load string
	if th.ToolProcsMax >= 0 {
		load = toolLoadLine(t, th.Tools, th.ToolProcsMax, owners)
	}
	w.check("toolprocs", load != "", "%s", load)

	var found []stack
	if th.StackMax > 0 {
		found = stacks(t, now, th.StackMax, owners)
	}
	cur := map[string]bool{}
	for _, s := range found {
		key := "stacked " + s.key
		cur[key] = true
		w.emit(key, "%s", s.line())
	}
	// The STACKED lines said, a restarted watch's among them.
	w.mu.Lock()
	var gone []string
	for key := range w.active {
		if strings.HasPrefix(key, "stacked ") && !cur[key] {
			gone = append(gone, key)
		}
	}
	w.mu.Unlock()
	for _, key := range gone {
		w.clear(key)
	}
}

// pollSessions does what the process table and the sessions in it tell.
func (w *watcher) pollSessions(ctx context.Context, since time.Time, t *proc.Table) {
	if ctx.Err() != nil {
		return
	}
	w.table = t
	sessions := discover(w.a.cfg, t, w.a.now)
	owners := make(map[int]string, len(sessions))
	for _, s := range sessions {
		owners[s.PID] = s.Name
	}
	w.owners.Store(&owners)
	w.suspends(ctx, since, sessions)
	w.kills(ctx, since, sessions, t)
	w.tendReporter(ctx, sessions)
	w.tendFeedback(ctx)
	if ctx.Err() != nil {
		return
	}
	w.pending(ctx, sessions)
	if ctx.Err() != nil {
		return
	}
	w.sessionChanges(sessions)
	w.staleLeases(ctx, sessions)
	w.unownedPages(ctx, sessions)
	w.runaways(sessions, t)
	w.twins(sessions, t)
	w.ungated(sessions, t)
	if ctx.Err() != nil {
		return
	}
	w.sameTitles(w.stopRevived(ctx, sessions))
	if ctx.Err() != nil {
		return
	}
	w.staleWatches(ctx, t)
}

// twinKey starts the condition key of a session that runs two CLIs, and
// twinGrace is how long the youngest of them runs before they count.
const (
	twinKey   = "twin "
	twinGrace = 30 * time.Second
)

// twins says each session that runs more than one Claude Code CLI on its
// session id (a headless turn beside its desktop CLI): both act on its task,
// each unaware of the other, and a message by name reaches only one. One
// TWIN CLI line naming every CLI's PID and directory, and its ENDED line once
// one CLI is left. Discover keeps one session per id, so the CLIs are counted
// in the process table; a restarted CLI overlaps its predecessor for a
// moment, so CLIs count once the youngest has run for twinGrace.
func (w *watcher) twins(sessions []*claude.Session, t *proc.Table) {
	names := map[string]string{}
	for _, s := range sessions {
		if s.ID != "" && s.Harness == "" {
			names[s.ID] = s.Name
		}
	}
	byID := map[string][]*proc.Process{}
	for _, p := range t.ByPID {
		if p.Comm != claudeComm {
			continue
		}
		for id := range names {
			if resumes(p.Args, id) || startsSession(p, id) {
				byID[id] = append(byID[id], p)
			}
		}
	}
	found := map[string]bool{}
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		cli := byID[id]
		if len(cli) < 2 || slices.ContainsFunc(cli, func(p *proc.Process) bool { return w.a.now.Sub(p.Start) < twinGrace }) {
			continue
		}
		slices.SortFunc(cli, func(a, b *proc.Process) int { return cmp.Compare(a.PID, b.PID) })
		var each []string
		for _, p := range cli {
			each = append(each, fmt.Sprintf("PID %d in %s", p.PID, t.Cwd(p.PID)))
		}
		key := twinKey + id
		found[key] = true
		w.emit(key, "TWIN CLI: %q runs %d CLIs on session %s (%s): both act on its task and a message by name reaches only one; stop the one that should not run",
			names[id], len(cli), id, strings.Join(each, ", "))
	}
	w.clearMissing(twinKey, found)
}

// poll does everything but the machine sample: the process table, the
// sessions, the lanes and the budget. It waits for a lane read or a budget
// probe for one watch.interval at most.
func (w *watcher) poll(ctx context.Context) {
	w.polls.Add(1)
	w.a.now = w.clk().Now()
	since := w.lastPoll
	w.lastPoll = w.a.now
	th := w.a.cfg.Watch

	t, err := plat.Machine.Processes()
	switch {
	case w.unavailable(secSessions, err):
		// No session is known: what reads them is left out, not guessed.
	case err != nil:
		w.fail("proc", "cannot read the process table: %v", err)
		return
	default:
		w.clear("proc")
		w.pollSessions(ctx, since, t)
	}
	if ctx.Err() != nil {
		return
	}
	w.staleMerges()
	w.lostMerges(ctx)
	w.owedOutcomes(ctx)
	w.a.pollToolWindow(ctx, watchParty)
	w.stalls()
	w.slotHolds()
	w.unbrokered()
	w.vaultWaits()
	if ctx.Err() != nil {
		return
	}
	w.sink(ctx)
	now := w.a.now
	if now.Sub(w.lastSettle) >= w.readEvery(th.Interval.Duration) {
		w.lastSettle = now
		w.inFlight(w.readCtx(ctx), th.Interval.Duration, &w.settling, func(ctx context.Context) {
			_ = w.a.checkPlaces(ctx, watchParty, now, "", "", 0, th.Interval.Duration)
			w.hungMerges(ctx, now)
			w.settled(ctx, now)
		})
	}
	if now.Sub(w.lastBudget) >= th.BudgetEvery.Duration {
		w.lastBudget = now
		w.inFlight(ctx, th.Interval.Duration, &w.budgeting, func(ctx context.Context) { w.budget(ctx, now) })
	}
	if now.Sub(w.lastKindSpend) >= w.readEvery(kindSpendEvery) {
		w.lastKindSpend = now
		// the day's transcripts are read whole: the poll does not wait
		w.inFlight(ctx, 0, &w.kindSumming, func(context.Context) { w.kindSpending(now) })
	}
	if now.Sub(w.lastTeleport) >= w.readEvery(th.Interval.Duration) {
		w.lastTeleport = now
		w.claudeLogin()
		w.inFlight(ctx, th.Interval.Duration, &w.teleporting, w.teleport)
	}
	if now.Sub(w.lastServing) >= w.readEvery(th.Interval.Duration) {
		w.lastServing = now
		// A drain of a self-hosted model waits for its sessions: the watch
		// runs the switch the constraint asked for once they are gone.
		w.inFlight(ctx, th.Interval.Duration, &w.serving, func(ctx context.Context) {
			_ = w.a.reconcileServeModes(ctx)
		})
	}
	if w.a.cfg.Central.Enabled() && now.Sub(w.lastCentral) >= w.readEvery(th.Interval.Duration) {
		w.lastCentral = now
		w.inFlight(ctx, th.Interval.Duration, &w.centraling, w.syncRoster)
	}
	if now.Sub(w.lastSweep) >= w.readEvery(w.a.cfg.Outbound.SweepEvery.Duration) {
		w.lastSweep = now
		// The sweep takes a while on a big home directory: the poll does not wait.
		w.inFlight(ctx, 0, &w.sweeping, func(context.Context) { w.exposures() })
	}
	if ctx.Err() != nil {
		return
	}
	w.saveMark()
	if w.notifier != nil {
		w.notifier.Flush(ctx, w.a.now)
	}
}

// teleport runs in the command, which holds what it needs (watchTeleport).
func (w *watcher) teleport(ctx context.Context) {
	watchTeleport(w, ctx)
}

// vaultKey starts the condition key of a call waiting on the vault, and
// vaultUnlockedKey is the broker's approval.
const (
	vaultKey         = "vault "
	vaultUnlockedKey = "vault-unlocked"
	vaultReadKey     = "vault-read"
	vaultSigninKey   = "vault-signin"
	vaultRetryKey    = "vault-retry"
	vaultDroppedKey  = "vault-dropped"
)

// vaultWaits says the vault's state (secret.session): one VAULT UNLOCKED
// line while the broker holds the session, until when, and its ENDED line
// when the broker forgets it; a VAULT SIGN-IN RETRYING line while the
// broker waits for the credential store or tries a failed sign-in again,
// and a VAULT SIGN-IN FAILED line with the broker's reason once it gave
// up; a VAULT SESSION DROPPED line while a session op stopped taking
// waits for its new sign-in; one VAULT LOCKED line for each call that waits on the sign-in,
// and once it goes on its ENDED line, or a line that it timed out with the
// vault still locked. Nothing asks the person.
func (w *watcher) vaultWaits() {
	if !w.a.cfg.Secret.Session {
		return
	}
	statePath, err := secret.StatePath()
	if err != nil {
		return
	}
	st, err := secret.ReadState(statePath)
	if err != nil {
		w.fail(vaultReadKey, "cannot read the vault's state: %v", err)
		return
	}
	w.check(vaultUnlockedKey, st.Unlocked, "VAULT UNLOCKED: the broker holds the vault session since %s until %s",
		st.Since.Local().Format("15:04"), st.Until.Local().Format("15:04"))
	w.check(vaultRetryKey, st.Retrying != "", "VAULT SIGN-IN RETRYING: %s", st.Retrying)
	w.check(vaultSigninKey, st.Error != "", "VAULT SIGN-IN FAILED: %s", st.Error)
	w.check(vaultDroppedKey, st.Dropped != "", "VAULT SESSION DROPPED at %s: op no longer took it (%s); the broker signs in again",
		st.DroppedAt.Local().Format("15:04"), st.Dropped)
	waitsPath, err := secret.WaitsPath()
	if err != nil {
		return
	}
	ws, err := secret.ReadWaits(waitsPath)
	if err != nil {
		w.fail(vaultReadKey, "cannot read the vault's waiting calls: %v", err)
		return
	}
	w.clear(vaultReadKey)
	found := map[string]bool{}
	for _, v := range ws {
		key := vaultKey + v.Who + " " + v.Ref
		found[key] = true
		w.emit(key, "VAULT LOCKED: %s waits on %s since %s; the broker signs in",
			v.Who, v.Ref, v.Since.Local().Format("15:04"))
	}
	if !st.Unlocked {
		// a call gone while the vault is locked gave up: no ENDED line
		for key, c := range w.drop(vaultKey, found) {
			who, ref := key[len(vaultKey):], ""
			if i := strings.LastIndexByte(who, ' '); i >= 0 {
				who, ref = who[:i], who[i+1:]
			}
			w.emitNow("vault", "VAULT LOCKED: %s's call on %s timed out (seen waiting since %s), still locked",
				who, ref, c.Since.Local().Format("15:04"))
		}
	}
	w.clearMissing(vaultKey, found)
}

// exposedKey starts the condition key of a credential exposed on disk.
const exposedKey = "exposed "

// exposures says each credential left exposed on disk (guard.Sweep), one
// EXPOSED line naming the file when it is found, never its content, and one
// ENDED line once it is fixed.
func (w *watcher) exposures() {
	ctx := w.a.context()
	o := w.a.cfg.Outbound
	found := map[string]bool{}
	for _, e := range guard.SweepContext(ctx, o.SweepRoots, o.SweepDepth) {
		key := exposedKey + e.Path
		found[key] = true
		w.emit(key, "EXPOSED %s: %s", e.Path, e.What)
	}
	if ctx.Err() != nil {
		return
	}
	w.clearMissing(exposedKey, found)
}

// budget runs in the command, which holds what it needs (watchBudget).
func (w *watcher) budget(ctx context.Context, now time.Time) {
	watchBudget(w, ctx, now)
}

// callersText runs in the command, which holds what it needs (watchCallersText).
func (w *watcher) callersText() string {
	return watchCallersText(w)
}

// stalls runs in the command, which holds what it needs (watchStalls).
func (w *watcher) stalls() {
	watchStalls(w)
}

// lostMerges records the outcome of each running merge whose gate process
// and devctl are gone (recordGone), one MERGE CUT, MERGE RECORDED or MERGE
// LOST line each; a cut run's owed outcome wakes its owner in the same poll
// (owedOutcomes). The gate itself prunes a lost merge only when the lane's
// next merge arrives, and until then the lane shows a merge running that no
// process runs.
func (w *watcher) lostMerges(ctx context.Context) {
	recorded, cut, lost := w.a.recordGone(ctx)
	for _, d := range cut {
		w.emitNow("lanes", "MERGE CUT: %s", d)
	}
	for _, d := range recorded {
		w.emitNow("lanes", "MERGE RECORDED: %s", d)
	}
	for _, m := range lost {
		w.emitNow("lanes", "MERGE LOST: %s in lane %s by %q: its gate (pid %d) and devctl are gone, whether it merged is unknown; the lane settles until %s, then frees once its HelmReleases are Ready",
			m.Key(), m.Lane, m.By.Name, m.PID, clock(w.a.now, w.a.now.Add(w.a.cfg.Merge.Settle.Duration)))
	}
}

// hungMerges runs in the command, which holds what it needs (watchHungMerges).
func (w *watcher) hungMerges(ctx context.Context, now time.Time) {
	watchHungMerges(w, ctx, now)
}

// settled drops the settling merges whose lane has settled, so lanes shows
// the lane free before its next merge starts: a lane with no installation
// has nothing to roll (a merge whose release is unknown settles it by the
// settle rule), and one whose release rolled and whose HelmReleases
// are Ready is done, however late. A merge not settled past
// merge.settleTimeout is one LANE STUCK line with what the lane waits for,
// and one ENDED line once it settles or the lane is cleared.
func (w *watcher) settled(ctx context.Context, now time.Time) {
	st, err := w.a.readSnapshot()
	if err != nil {
		return
	}
	type reading struct {
		hrs []merge.HelmRelease
		err error
	}
	read := map[string]reading{}
	done := map[string]string{}
	stuck := map[string]bool{}
	for _, m := range st.Merges {
		if m.Phase != state.Settling {
			continue
		}
		lane, ok := w.a.cfg.LaneNamed(m.Lane)
		if !ok || lane.Installation == "" {
			// A release its gate confirms settles with the verdict, one
			// unknown by the settle rule: it may be cutting.
			if merge.Confirming(m, proc.Alive) {
				continue
			}
			if ready, _ := merge.Ready(lane, nil, &m, now, w.a.cfg.Merge.Settle.Duration); ready {
				done[m.Key()] = "no installation to roll"
			}
			continue
		}
		r, ok := read[lane.Name]
		if !ok {
			r.hrs, r.err = w.helmReleases(ctx, lane)
			read[lane.Name] = r
		}
		why := ""
		if r.err != nil {
			why = fmt.Sprintf("the HelmReleases of %s cannot be read (%v)", lane.Installation, r.err)
		} else if ready, wait := merge.Ready(lane, r.hrs, &m, now, w.a.cfg.Merge.Settle.Duration); ready {
			done[m.Key()] = "rolled, HelmReleases of " + lane.Installation + " Ready"
			if wait != "" {
				done[m.Key()] = "settled without a roll, " + wait
			}
			continue
		} else {
			why = wait
		}
		if since := now.Sub(m.Finished); since > w.a.cfg.Merge.SettleTimeout.Duration {
			key := "stuck:" + lane.Name + ":" + m.Key()
			stuck[key] = true
			if m.Release == "" && !m.ReleaseFailed {
				why += fmt.Sprintf("; recover with devctl release wait %s --pr %d, then name the release with beekeeper lanes settle %s %d --release <version>", m.Repo, m.PR, m.Repo, m.PR)
			}
			w.emit(key, "LANE STUCK %s: %s has not settled %s after its merge: %s; fix the installation or clear the lane (beekeeper lanes clear %s)",
				lane.Name, m.Key(), dur(since), why, lane.Name)
		}
	}
	w.clearMissing("stuck:", stuck)
	if len(done) == 0 {
		return
	}
	var settled []state.Merge
	_ = w.a.store.Update(func(st *state.State) ([]state.Event, error) {
		var ev []state.Event
		st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool {
			why, ok := done[m.Key()]
			if !ok || m.Phase != state.Settling {
				return false
			}
			settled = append(settled, m)
			ev = append(ev, event(watchParty, verbLaneSettled, "%s: %s %s", m.Lane, m.Key(), why))
			return true
		})
		return ev, nil
	})
	for _, m := range settled {
		w.a.leaveCentral(ctx, []state.Merge{m}, done[m.Key()])
	}
}

// kills runs in the command, which holds what it needs (watchKills).
func (w *watcher) kills(ctx context.Context, since time.Time, sessions []*claude.Session, t *proc.Table) {
	watchKills(w, ctx, since, sessions, t)
}

func (w *watcher) sessionChanges(sessions []*claude.Session) {
	cur := map[string]*claude.Session{}
	for _, s := range sessions {
		cur[s.Key()] = s
	}
	if w.sessions == nil {
		w.sessions = cur
		return
	}
	var started, ended, restarted, quiet []string
	for k, s := range cur {
		prev, ok := w.sessions[k]
		switch {
		case !ok:
			started = append(started, s.Name)
		case prev.PID != s.PID:
			restarted = append(restarted, s.Name)
		}
	}
	for k, s := range w.sessions {
		_, ok := cur[k]
		recorded := slices.ContainsFunc(w.records, func(r state.Record) bool { return r.Session.Is(s.Party()) })
		if !ok && !recorded {
			ended = append(ended, s.Name)
		}
	}
	for _, c := range []struct {
		what  string
		names []string
	}{{"started", started}, {"ended", ended}, {"restarted", restarted}} {
		loud, held := w.quietSessions(c.names)
		if len(loud) > 0 {
			w.emitNow("sessions", "SESSIONS %s: %s", c.what, quoted(loud))
		}
		if len(held) > 0 {
			quiet = append(quiet, fmt.Sprintf("SESSIONS %s: %s (quiet: watch.quietSessions)", c.what, quoted(held)))
		}
	}
	w.a.logQuiet(quiet...)
	w.sessions = cur
}

// quoted is the names quoted and sorted, comma-separated.
func quoted(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%q", n)
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}

// quietSessions splits session names into those that wake and the
// short-lived ones in watch.quietSessions.
func (w *watcher) quietSessions(names []string) (loud, quiet []string) {
	for _, n := range names {
		if slices.ContainsFunc(w.a.cfg.Watch.QuietSessions, func(p string) bool { return alerts.Glob(p, n) }) {
			quiet = append(quiet, n)
		} else {
			loud = append(loud, n)
		}
	}
	return loud, quiet
}

// watchParty is who the watch's events are by.
var watchParty = state.Party{Name: "beekeeper watch"}

// pending prints the notes and timers that fell due, the recorded sessions
// that ended, a relay taken or expired and the relay due at relayAt.
// The state keeps that they were reported, so each is one line however many
// watches run; the state is written only then.
func (w *watcher) pending(ctx context.Context, sessions []*claude.Session) {
	if ctx.Err() != nil {
		return
	}
	st, err := w.a.readSnapshot()
	if err != nil {
		w.fail("state-read", "cannot read the state: %v", err)
		return
	}
	w.clear("state-read")
	if w.chores {
		defer func() {
			if ctx.Err() == nil {
				w.claims(ctx)
			}
		}()
	}
	st = w.syncExecutions(ctx, st)
	if ctx.Err() != nil {
		return
	}
	w.records = st.Records
	supervised := w.checkLoop(ctx, st)
	if ctx.Err() != nil {
		return
	}
	if w.standby {
		if !w.guideRecovery {
			w.guideGone(ctx, st, sessions)
			w.resumeRestarted(ctx, guideRole, st, sessions)
		}
		for _, rl := range roles {
			w.relayOverdue(ctx, rl, st, sessions)
		}
		w.importRows(ctx, st)
	}
	if !w.standby || !supervised {
		w.guideMonitor(ctx, st, sessions)
	}
	if w.standby && supervised {
		return // the supervisor loop's watch reports them
	}
	if ctx.Err() != nil {
		return
	}
	w.sayStop(st)
	w.noteAnswers(st)
	w.refusals()
	u := w.watchUsage(ctx, st, sessions)
	if ctx.Err() != nil {
		return
	}
	if w.chores && (u == nil || !u.High) {
		w.autoResume(ctx, st) // the parks an earlier poll marked resumable
	}
	if w.chores {
		w.stopParked(ctx, st, sessions) // a parked session costs no memory
	}
	if st.Stop == nil {
		w.stoppedAgents(st, sessions) // STOPPED names the sessions the stop ended
	}
	if ctx.Err() != nil {
		return
	}
	w.capacity(ctx, st, sessions)
	w.importWaits(st)
	w.rowlessAgents(st)
	w.handoversDue(st, sessions)
	w.doctor(ctx)
	if ctx.Err() != nil {
		return
	}
	signedIn := probeLogins(ctx, st.Notes)
	probed := probeHoldLifts(ctx, st.Holds, w.a.now)
	if ctx.Err() != nil {
		return
	}
	var over, kept []overtake
	if len(overtakeRefs(st)) > 0 {
		readRefs := func() error {
			over, kept = w.overtakenNow(ctx, st)
			return nil
		}
		if w.loopOwned {
			if err := w.a.loopGitHub(ctx, "watch", readRefs); err != nil {
				over, kept = nil, nil
			}
		} else {
			_ = readRefs()
		}
	}
	if ctx.Err() != nil {
		return
	}
	parks := w.settledParks(ctx, st)
	if ctx.Err() != nil {
		return
	}
	if !w.chores {
		w.wouldOvertake(st, over, kept)
		over, kept = nil, nil
	}
	githubHeld := loop.BoardBudgetLow(st.Budget, w.a.cfg.GitHub.Floor, w.a.now) || w.a.resetFirst(st)
	var found map[int]checkResult
	if w.loopOwned {
		found = w.a.loopTimerChecks(ctx, st.Timers, w.a.now, githubHeld)
	} else {
		found = checkTimers(ctx, st.Timers, w.a.now, githubHeld)
	}
	if ctx.Err() != nil {
		return
	}
	if w.chores {
		w.overrides(ctx, githubHeld)
	}
	if ctx.Err() != nil {
		return
	}
	var settles func(state.Timer) bool // nil: a running watch settles every timer
	if !w.chores {
		w.wouldFireTimers(st, found)
		settles = func(state.Timer) bool { return false }
	}
	var fires []timerFire
	var defaulted, overtaken []state.Note
	fire := func(st *state.State) ([]string, []state.Event, bool) {
		lines, evs := closeProbed(st, signedIn)
		hl, he := liftProbed(st, probed)
		lines, evs = append(lines, hl...), append(evs, he...)
		nl, ne, nd := closeDefaulted(st, w.a.cfg.Guide.Person, watchParty, w.a.now)
		lines, evs, defaulted = append(lines, nl...), append(evs, ne...), nd
		overtaken = heldAmong(st.Notes, over)
		ol, oe := closeOvertaken(st, over, watchParty)
		lines, evs = append(lines, ol...), append(evs, oe...)
		ol, oe = keepOpen(st, kept, watchParty)
		lines, evs = append(lines, ol...), append(evs, oe...)
		kl, ke := markResumable(st, w.a.recheckLeaseParks(st, parks), w.a.cfg.Agents.AutoResume, w.a.now)
		lines, evs = append(lines, kl...), append(evs, ke...)
		tl, te, tf, touched := settleTimersMatching(st, found, w.a.now, w.a.cfg.Watch.Interval.Duration, settles)
		lines, evs, fires = append(lines, tl...), append(evs, te...), tf
		seen := touched
		pl, pe := firePending(st, sessions, w.a.now)
		lines, evs = append(lines, pl...), append(evs, pe...)
		ppl, ppe := parkedNews(st, w.a.cfg.Guide.Person, w.a.now)
		lines, evs = append(lines, ppl...), append(evs, ppe...)
		return lines, evs, seen || len(lines) > 0 || len(evs) > 0
	}
	if _, _, changed := fire(st); !changed {
		return
	}
	var lines []string
	var due []dueItem
	var verdicts []state.WorkerReport
	err = w.a.store.Update(func(st *state.State) ([]state.Event, error) {
		var evs []state.Event
		verdicts = slices.DeleteFunc(slices.Clone(st.WorkerReports), func(r state.WorkerReport) bool { return r.TellTo == (state.Party{}) })
		lines, evs, _ = fire(st)
		for _, l := range lines {
			evs = append(evs, event(watchParty, "watch.pending", "%s", l))
		}
		w.records = st.Records
		due = firedNow(st, w.a.now)
		return evs, nil
	})
	if err != nil {
		w.fail("state-write", "cannot write the state: %v", err)
		return
	}
	w.clear("state-write")
	for _, l := range lines {
		w.outputEvent(l)
	}
	w.tellReviewVerdicts(ctx, verdicts)
	for _, n := range defaulted {
		due = append(due, defaultedItem(&n))
	}
	for _, d := range due {
		w.notify(ctx, notify.Due, d.key, d.summary, d.body)
	}
	for _, f := range fires {
		if f.t.Wake == "" && f.t.Run == "" {
			w.notify(ctx, notify.Due, fmt.Sprintf("timer#%d", f.t.ID), fmt.Sprintf("beekeeper: timer #%d: %s", f.t.ID, truncate(f.reason, 60)), truncate(f.t.What, 200))
		}
	}
	w.actTimers(ctx, fires)
	if len(defaulted) > 0 {
		w.timerActs.Go(func() {
			w.tellDefaulted(ctx, defaulted)
			for _, n := range defaulted {
				w.emitReleased(ctx, &n, closedDefaulted)
			}
		})
	}
	for _, n := range overtaken {
		w.timerActs.Go(func() { w.emitReleased(ctx, &n, closedOvertaken) })
	}
}

// emitReleased releases the board items the closed decision n held and
// says so, one line each.
func (w *watcher) emitReleased(ctx context.Context, n *state.Note, how string) {
	for _, l := range w.a.release(ctx, watchParty, n, how) {
		w.emitNow("pending", "%s", l)
	}
}

// noteAnswers says the answers of the notes that closed since the last
// tick, once: NOTE ANSWERED. The log is read only when a note closed.
func (w *watcher) noteAnswers(st *state.State) {
	open := make(map[int]bool, len(st.Notes))
	for _, n := range st.Notes {
		open[n.ID] = true
	}
	prev := w.openNotes
	w.openNotes = open
	if prev == nil {
		w.answersSince = w.a.now
		return
	}
	closed := false
	for id := range prev {
		closed = closed || !open[id]
	}
	if !closed {
		return
	}
	since := w.answersSince
	evs, err := w.a.store.Events(0, func(e state.Event) bool { return e.Verb == answerRecorded && e.At.After(since) })
	if err != nil {
		return // the next close reads them again
	}
	for _, e := range evs {
		w.answersSince = e.At
		if an, ok := parseAnswered(e); ok && an.ID != 0 && prev[an.ID] {
			w.emitNow("pending", "%s", answeredLine(an.ID, cmp.Or(e.By.Person, e.By.Name), truncate(an.Answer, 300)))
		}
	}
}

// refusals reports upstream refusals and recovered hook panics since the
// last reading. The first reading establishes a baseline and says none.
func (w *watcher) refusals() {
	evs, err := w.a.store.Follow("watch.refused", func(e state.Event) bool { return e.Verb == verbHookRefused || e.Verb == "hook.panic" })
	if err != nil {
		return // the next reading reads them again
	}
	since := w.refusedSince
	if since.IsZero() {
		w.refusedSince = w.a.now
		return
	}
	for _, e := range evs {
		if e.At.After(since) {
			w.refusedSince = e.At
			if e.Verb == "hook.panic" {
				w.emitNow("hook.panic", "HOOK PANIC %s", truncate(e.Detail, 400))
				continue
			}
			w.emitNow("refused", "REFUSED %s: %s", cmp.Or(e.By.Name, e.By.Session), truncate(e.Detail, 400))
		}
	}
}

// stoppedAgents says once which agents with a task have no running CLI: a
// worker stopped by a reboot or a crash does not come back by itself, and
// its task waits until the supervisor resumes it.
func (w *watcher) stoppedAgents(st *state.State, sessions []*claude.Session) {
	stopped := map[string]bool{}
	var now []string
	for _, ag := range stoppedAgents(st.Agents, sessions) {
		if st.Report.Running() && ag.Is(st.Report.Party) {
			continue // the reporter's turn ended: tendReporter ends it, nobody resumes it
		}
		if keptBy(st, ag, w.a.now) != "" {
			continue // parked on purpose: a person or its timer brings it back
		}
		k := cmp.Or(ag.Session, ag.Name)
		last, gone := goneAgent(w.a.cfg, st, sessions, ag, w.a.cfg.Agents.StaleAfter.Duration, w.a.now)
		if gone {
			k += "/gone"
		}
		stopped[k] = true
		if !w.stopped[k] {
			hint := resumeHint(ag)
			switch on := parkedOn(waitsOf(st.Records, ag), w.a.cfg.Guide.Person); {
			case ag.Undelivered != "":
				hint = fmt.Sprintf("task not delivered: %s; beekeeper agents wake %q <message> delivers it", ag.Undelivered, ag.Name)
			case ag.Import.Pending(w.a.now):
				hint = importStatus(ag, w.a.now)
			case on != "":
				hint = fmt.Sprintf("parked on a person: %s; their answer resumes it", on)
			case resumedForTask(ag):
				hint = fmt.Sprintf("ended on a wait again after its resume at %s on %s: %s", clock(w.a.now, ag.ResumedWait), ag.ResumedOn, hint)
			}
			if gone {
				hint = goneActivityPrefix + last.UTC().Format(time.RFC3339) + "; doctor retires it; " + hint
				if qerr := agyQuotaError(w.a.cfg, ag); qerr != nil {
					hint = qerr.Error() + "; slot freed; doctor retires it and supervisor re-queues its task"
				}
			}
			now = append(now, fmt.Sprintf("%q (%s)", ag.Name, hint))
		}
	}
	w.stopped = stopped
	if len(now) > 0 {
		w.emitNow("agents", "AGENTS STOPPED with a task and no running CLI: %s", strings.Join(now, ", "))
	}
}

// importWaits says once per wait which reopen waits to show its agent in
// the desktop, on what and until when: an import held by a person working
// in the desktop's window shows here instead of as an agent that never
// comes back.
func (w *watcher) importWaits(st *state.State) {
	waits := map[string]time.Time{}
	var now []string
	for _, ag := range st.Agents {
		if !ag.Import.Pending(w.a.now) {
			continue
		}
		k := cmp.Or(ag.Session, ag.Name)
		waits[k] = ag.Import.Since
		if !w.waits[k].Equal(ag.Import.Since) {
			now = append(now, fmt.Sprintf("%q (%s)", ag.Name, importStatus(ag, w.a.now)))
		}
	}
	w.waits = waits
	if len(now) > 0 {
		w.emitNow("agents-import", "IMPORT WAITS: %s", strings.Join(now, ", "))
	}
}

// dueItem is a note or timer this poll reported due.
type dueItem struct{ key, summary, body string }

// defaultedItem is the notification of a decision closed with its default.
func defaultedItem(n *state.Note) dueItem {
	body := truncate(n.Text, 200)
	if n.For != "" {
		body = "for " + n.For + ": " + body
	}
	return dueItem{fmt.Sprintf("note#%d", n.ID), fmt.Sprintf("beekeeper: note #%d defaulted", n.ID), body + "\napplied: " + truncate(n.Default, 120)}
}

// firedNow are the notes and timers fired at now.
func firedNow(st *state.State, now time.Time) []dueItem {
	var out []dueItem
	for _, n := range st.Notes {
		if n.Fired.Equal(now.UTC()) {
			body := truncate(n.Text, 200)
			if n.For != "" {
				body = "for " + n.For + ": " + body
			}
			if n.Default != "" {
				body += "\nif unanswered: " + truncate(n.Default, 120)
			}
			out = append(out, dueItem{fmt.Sprintf("note#%d", n.ID), fmt.Sprintf("beekeeper: note #%d due", n.ID), body + "\nbeekeeper note list"})
		}
	}
	for _, t := range st.Timers {
		if t.Fired.Equal(now.UTC()) {
			out = append(out, dueItem{fmt.Sprintf("timer#%d", t.ID), fmt.Sprintf("beekeeper: timer #%d due: %s", t.ID, truncate(t.What, 60)),
				truncate(t.What, 200) + fmt.Sprintf("\nbeekeeper timer done %d", t.ID)})
		}
	}
	return out
}

// loopDownGrace is how long the supervisor loop may be down before a watch
// says so: a restart of its service, or one the standby watch makes, is
// back within it.
const loopDownGrace = 2 * time.Minute

// loopRestartEvery is how often the standby watch restarts the supervisor
// loop's service while the loop stays down.
const loopRestartEvery = time.Minute

// checkLoop checks the supervisor, which is the supervisor loop, and
// reports whether it runs. While it does not the standby watch restarts
// its service (never during a stop) at most every loopRestartEvery, and
// once it stayed down past loopDownGrace every watch says SUPERVISOR LOOP
// DOWN and notifies (no-supervisor) until it runs again. No session is
// ever started or relayed for it.
func (w *watcher) checkLoop(ctx context.Context, st *state.State) bool {
	if w.standby {
		w.a.rearmRecovery()
	}
	if line, err := w.a.checkDeliveries(w.a.now); err != nil {
		w.fail("delivery", "cannot read delivery health")
	} else {
		w.clear("delivery")
		if line != "" {
			w.emitNow("delivery", "%s", line)
		}
	}
	if line, err := w.a.checkThroughput(w.a.now, false); err != nil {
		w.fail(watchThroughputKey, "release throughput unknown: cannot read durable health")
	} else {
		w.clear(watchThroughputKey)
		if line != "" {
			w.emitNow(watchThroughputKey, "%s", line)
		}
	}
	if w.loopRunning() {
		if w.stand.loopDown {
			w.emitNow("supervisor", "SUPERVISOR LOOP BACK: the supervisor loop runs again (down since %s)", clock(w.a.now, w.stand.loopSince))
		}
		w.stand.loopDown, w.stand.loopSince = false, time.Time{}
		return true
	}
	if st.Stop != nil {
		// A stop holds the loop too: nothing restarts it, sayStop reports.
		w.stand.loopSince = time.Time{}
		return false
	}
	if w.stand.loopSince.IsZero() {
		w.stand.loopSince = w.a.now
	}
	restart := ""
	if w.standby {
		restart = w.restartLoop(ctx)
	}
	if w.a.now.Sub(w.stand.loopSince) < loopDownGrace {
		return false
	}
	key := "loop@" + w.stand.loopSince.UTC().Format(time.RFC3339)
	l := fmt.Sprintf("SUPERVISOR LOOP DOWN: the supervisor loop does not run since %s; no pull request is reviewed or merged on its go and no worker starts until it runs (%s)%s",
		clock(w.a.now, w.stand.loopSince), loopServiceHint(), restart)
	if !w.stand.loopDown {
		w.stand.loopDown = true
		w.emitNow("supervisor", "%s", l)
	}
	w.notify(ctx, notify.NoSupervisor, key, "beekeeper: the supervisor loop is down", l)
	return false
}

// unitComingUp says a unit in this state runs or is on its way there.
func unitComingUp(unitState string) bool {
	return unitState == unitActive || unitState == "activating" || unitState == "reloading"
}

// restartLoop starts the supervisor loop's service again (standby watch),
// at most every loopRestartEvery and only while the service is not
// starting already. It returns what the DOWN line adds.
func (w *watcher) restartLoop(ctx context.Context) string {
	unit := w.stand.loopUnit
	if unit == nil {
		return "; this watch restarts no service here (only the standby, on Linux with systemd)"
	}
	if unitComingUp(unit.state(ctx)) {
		return "; its service is starting"
	}
	if !w.stand.loopRestart.IsZero() && w.a.now.Sub(w.stand.loopRestart) < loopRestartEvery {
		return fmt.Sprintf("; the standby watch restarted its service at %s", clock(w.a.now, w.stand.loopRestart))
	}
	w.stand.loopRestart = w.a.now
	if err := unit.start(ctx); err != nil {
		w.emitNow("supervisor-restart", "SUPERVISOR LOOP RESTART FAILED: %s: %v (beekeeper install installs it)", loopUnit, err)
		return fmt.Sprintf("; restarting its service failed: %v", err)
	}
	w.emitNow("supervisor-restart", "SUPERVISOR LOOP RESTARTED: the standby watch started %s again", loopUnit)
	return "; the standby watch restarted its service"
}

// loopService reads and starts the supervisor loop's user service.
type loopService struct {
	state func(ctx context.Context) string
	start func(ctx context.Context) error
}

// firePending marks what is due or ended in st as reported and returns its
// watch lines and events; an event without a line is a recorded session
// that runs again.
func firePending(st *state.State, sessions []*claude.Session, now time.Time) ([]string, []state.Event) {
	var lines []string
	var evs []state.Event
	for i := range st.Notes {
		n := &st.Notes[i]
		if !state.Due(n.Due, n.Fired, now) {
			continue
		}
		n.Fired = now.UTC()
		l := fmt.Sprintf("NOTE DUE: #%d", n.ID)
		if n.For != "" {
			l += " for " + n.For
		}
		l += fmt.Sprintf(", due %s: %s", clock(now, n.Due), truncate(n.Text, 200))
		if n.Default != "" {
			l += "; if unanswered: " + truncate(n.Default, 120)
		}
		lines = append(lines, l)
		evs = append(evs, event(watchParty, "note.due", "#%d %s", n.ID, n.Text))
	}
	for i := range st.Timers {
		t := &st.Timers[i]
		if t.Auto() || !state.Due(t.Due, t.Fired, now) {
			continue
		}
		t.Fired = now.UTC()
		lines = append(lines, fmt.Sprintf("TIMER: #%d due %s: %s (beekeeper timer done %d)", t.ID, clock(now, t.Due), truncate(t.What, 200), t.ID))
		evs = append(evs, event(watchParty, "timer.due", "#%d %s", t.ID, t.What))
	}
	for i := range st.Records {
		r := &st.Records[i]
		_, live := claude.Live(sessions, r.Session)
		if live && !r.Ended.IsZero() {
			r.Ended = time.Time{} // resumed: its next end is reported again
			evs = append(evs, event(watchParty, "session.resumed", "%s: %s", r.Session.Name, recordText(*r)))
		}
		if live || !r.Ended.IsZero() || rosterKept(st, r.Session, now) != "" {
			continue
		}
		r.Ended = now.UTC()
		lines = append(lines, fmt.Sprintf("SESSION ENDED: %q, which %s: re-query %s", r.Session.Name, truncate(recordText(*r), 200), r.Issue))
		evs = append(evs, event(watchParty, "session.ended", "%s: %s", r.Session.Name, recordText(*r)))
	}
	for _, r := range st.WorkerReports {
		lines = append(lines, reportLines(r)...)
	}
	st.WorkerReports = nil
	return lines, evs
}

// tellReviewVerdicts wakes who started each review with its verdict: the
// verdict may be a plain comment on GitHub, which shows no review state. A
// wake that fails is said: the report above still carries the verdict.
func (w *watcher) tellReviewVerdicts(ctx context.Context, reports []state.WorkerReport) {
	for _, r := range reports {
		to := cmp.Or(r.TellTo.Session, r.TellTo.HostSession, r.TellTo.Name)
		msg := fmt.Sprintf("The desk review %q reports:\n%s", r.By.Name, r.Text)
		if err := w.a.wakeAgent(ctx, watchParty, to, msg, ""); err != nil {
			w.emitNow("pending", "REVIEW VERDICT NOT DELIVERED to %q from %q: %v", r.TellTo.Name, r.By.Name, err)
		}
	}
}

// reportLines are a worker's final report and each problem it found, a line
// of its own for the supervisor to file as an issue and hand to a worker.
func reportLines(r state.WorkerReport) []string {
	var lines []string
	if r.Text != "" {
		l := fmt.Sprintf("WORKER REPORT by %q", r.By.Name)
		if r.Task != "" {
			l += " (task: " + truncate(r.Task, 80) + ")"
		}
		lines = append(lines, l+": "+truncate(r.Text, 2000))
	}
	for _, p := range r.Problems {
		next := "file it and hand it to a worker"
		if superloop.Filed(p) {
			next = "filed; hand it to a worker"
		}
		lines = append(lines, fmt.Sprintf("PROBLEM FOUND by %q: %s; %s", r.By.Name, truncate(p, 400), next))
	}
	return lines
}

// watchParkOnPerson reports whether p names a note or an issue or pull request
// waiting on a person's word. Operational targets and untargeted waits do
// not become person waits merely because their descriptions mention a person.
func watchParkOnPerson(p *state.Park, person string) bool {
	if _, note := parkNote(p.On); note {
		return parkOnPerson(p, person)
	}
	if _, _, note := parkCentralNote(p.On); note {
		return parkOnPerson(p, person)
	}
	if _, ref := parseRef(p.On); !ref {
		return false
	}
	return parkOnPerson(p, person)
}

// parkOnPerson conservatively protects human waits from automatic wakes,
// including waits without a structured target.
func parkOnPerson(p *state.Park, person string) bool {
	if p.Checks {
		return false
	}
	// Desk reviews belong to the supervisor loop, including an unproven
	// review park that the worker still needs to correct.
	if strings.Contains(strings.ToLower(p.Waits), "desk review") {
		return false
	}
	if _, note := parkNote(p.On); note {
		return true
	}
	if _, _, note := parkCentralNote(p.On); note {
		return true
	}
	if _, lease := parkLease(p.On); lease {
		return false
	}
	return personWaitWords.MatchString(p.Waits) || namesPerson(p.Waits, person)
}

// parkedNews says once per park which agents parked on a person (a note,
// the person's or a colleague's answer, a review), which the supervisor
// carries to the guide, who tells the person, and which parked on a lease,
// with their place in its queue: the supervisor's grant settles that one.
func parkedNews(st *state.State, person string, now time.Time) ([]string, []state.Event) {
	var lines []string
	var evs []state.Event
	for i := range st.Agents {
		ag := &st.Agents[i]
		p := ag.Park
		if p == nil || !p.Told.IsZero() {
			continue
		}
		if res, ok := parkLease(p.On); ok {
			p.Told = now.UTC()
			lines = append(lines, fmt.Sprintf("PARKED ON A LEASE: %q %s; %s", ag.Name, parkedText(p, now), leaseQueueText(st, ag.Party, res)))
			evs = append(evs, event(watchParty, "agents.parked-on-lease", "%s: %s", ag.Name, res))
			continue
		}
		if !watchParkOnPerson(p, person) {
			continue
		}
		p.Told = now.UTC()
		lines = append(lines, fmt.Sprintf("PARKED ON A PERSON: %q %s; tell the guide, who tells %s", ag.Name, parkedText(p, now), cmp.Or(person, "the person")))
		evs = append(evs, event(watchParty, "agents.parked-on-person", "%s: %s", ag.Name, p.Waits))
	}
	return lines, evs
}

// leaseQueueText says where party stands in the grant queue of the lease
// res: its number and who is first, or that it holds no grant yet.
func leaseQueueText(st *state.State, party state.Party, res string) string {
	queue := lease.Pending(st, res, true, time.Time{}, 0)
	pos := slices.IndexFunc(queue, func(g state.Grant) bool { return g.To.Is(party) })
	switch {
	case pos < 0:
		return fmt.Sprintf("no grant of %s to it yet: the supervisor grants it, not the person", res)
	case pos == 0:
		return fmt.Sprintf("number 1 in the queue of %s, granted, for the lease to come free", res)
	}
	return fmt.Sprintf("number %d in the queue of %s behind %q: the supervisor's grant settles it, not the person", pos+1, res, queue[0].To.Name)
}

// runaways runs in the command, which holds what it needs (watchRunaways).
func (w *watcher) runaways(sessions []*claude.Session, t *proc.Table) {
	watchRunaways(w, sessions, t)
}

// slotHolds says each build slot a run has held longer than
// memcap.holdBound: one SLOT HELD LONG line naming the slot, its holder and
// the command when it starts, one ENDED line when the slot is free again.
func (w *watcher) slotHolds() {
	bound := w.a.cfg.Memcap.HoldBound.Duration
	if bound <= 0 {
		return
	}
	found := map[string]bool{}
	for _, s := range machine.ReadSlots(w.a.slotView()) {
		h, ok := guard.ParseHolder(s.Holder)
		if s.Free || !ok || h.Since.IsZero() || w.a.now.Sub(h.Since) < bound {
			continue
		}
		key := fmt.Sprintf("slothold:%d:%d:%s", s.N, h.PID, h.Since.UTC().Format(time.RFC3339))
		found[key] = true
		w.emit(key, "SLOT HELD LONG: slot %d past memcap.holdBound (%s), held by %s — a benchmark or a lab proof runs with beekeeper run --class benchmark|lab, which takes no slot",
			s.N, dur(bound), guard.HolderText(s.Holder, w.a.now))
	}
	w.clearMissing("slothold:", found)
}

// staleLeases runs in the command, which holds what it needs (watchStaleLeases).
func (w *watcher) staleLeases(ctx context.Context, sessions []*claude.Session) {
	watchStaleLeases(w, ctx, sessions)
}
