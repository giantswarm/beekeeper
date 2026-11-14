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

type Watcher struct {
	Host *app
	// batchEvery is immutable once the loops start; batch is guarded by mu.
	BatchEvery time.Duration
	batch      []string
	// loopOwned leaves event actions to the deterministic supervisor.
	LoopOwned bool
	Mu        sync.Mutex
	Last      map[string]time.Time
	// active are the lasting conditions said and not yet ENDED.
	Active map[string]Condition
	// markFile keeps active and reported for a restarted watch; dirty is
	// set when they changed since it was written.
	MarkFile string
	Dirty    bool
	// feedID is the id of the last central feed event the watch printed,
	// kept in its mark.
	FeedID          string
	lastPoll        time.Time
	SuspendEntry    time.Time
	SuspendChecked  time.Time
	SuspendThrough  time.Time
	SuspendRecovery map[string]SuspendWake
	SuspendDeliver  func(context.Context, SuspendWake) error
	// Hung checks get a fresh grace period on startup and after a suspend
	// (or a long gap in observations). These times stay in memory so their
	// monotonic readings can exclude time spent asleep.
	LastHungCheck time.Time
	HungGrace     time.Time
	// refusedSince is the newest hook.refused event said; zero before the
	// first reading, which said none.
	RefusedSince time.Time
	LastBudget   time.Time
	// lastKindSpend is when the day's spend per task kind was last summed,
	// kindSumming set while a sum runs.
	lastKindSpend time.Time
	kindSumming   atomic.Bool
	scopeOOM      int64
	Sessions      map[string]*claude.Session
	SeenKills     map[string]bool
	// reported are the stale leases said once.
	Reported map[string]bool
	// notifier sends the events that need a person (--notify); nil prints only.
	Notifier *notify.Notifier
	// swapSamples are watch.swapWindow's swap readings, oldest first: the
	// growth rate toward systemd-oomd's trigger.
	SwapSamples []swapSample
	// swap is the machine sample's latest swap reading, under mu: the
	// poll's CAPACITY lines weigh a start against it.
	swap *capacity.SwapReading
	// readHeadroom reads the headroom of a start; nil is app.readHeadroom.
	ReadHeadroom func(context.Context, *capacity.SwapReading) *capacity.Headroom
	// standby leaves a running supervisor's events to its watch.
	Standby bool
	// guideRecovery owns crash recovery on its own clock in a continuous watch.
	guideRecovery bool
	// upgrades are the last upgrade readings of the installations.
	Upgrades        upgrade.Readings
	UpgradeFailures map[string]int
	// noContextSaid are the readers (ALERTS, UPGRADES) whose installations
	// without a kube context this watch said.
	noContextSaid map[string]bool
	// records are the session records of the last poll: their sessions'
	// ends get the record's line instead of the SESSIONS ended one.
	Records []state.Record
	// stopped are the agents with a task this watch said have no running
	// CLI, by session key, until their CLI runs again.
	stopped map[string]bool
	// rowlessSaid are the workers this watch said have no row in the
	// desktop, by session, until they get one.
	RowlessSaid map[string]bool
	// waits are the reopen waits this watch said, by agent session and the
	// wait's start, until they end.
	waits map[string]time.Time
	// stand is the standby watch's memory of its messages, successors and
	// reopens; table the last poll's process table.
	Stand standbyWatch
	Table *proc.Table
	// feedbackNext is when the feedback watch reads the report threads
	// next; feedbackBusy is set while a read runs.
	FeedbackNext time.Time
	FeedbackBusy atomic.Bool
	// deliver sends a reply to an agent; nil is agents wake's.
	Deliver func(ctx context.Context, q, msg string) error
	// musterRun runs muster for the feedback watch; nil is the binary.
	musterRun central.Runner
	// runReport launches a reporter's turn; nil is launchReport.
	RunReport func(unit, id, name, prompt string) error
	// turnEnded reports whether a reporter's unit ended; nil is unitEnded.
	TurnEnded func(context.Context, string) bool
	// readHRs reads a lane installation's HelmReleases; nil is kubectl.
	ReadHRs func(context.Context, config.Lane) ([]merge.HelmRelease, error)
	// clock paces the loops and dates the polls; nil is the wall clock.
	Clock watchClock
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
	Disk DiskWatch
	// readForks reads the fork counter; nil is plat.Machine.Forks.
	ReadForks func() (uint64, error)
	// self is the watch's PID in the process table; 0 is its own.
	Self int
	// owners names the session of each CLI PID the last poll found: the
	// machine sample attributes a storm or a stack with it.
	Owners atomic.Pointer[map[int]string]
	// strained is the machine sample's verdict that the CPU is saturated:
	// the installation reads slow down (READS SLOWED).
	strained atomic.Bool
	// settling, budgeting and usageBusy are set while a lane read, a budget
	// probe or a subscription usage read runs: the next poll skips it instead
	// of queueing a second one.
	settling, budgeting, usageBusy atomic.Bool
	lastSettle                     time.Time
	lastUsage                      time.Time
	UsageErr                       string
	UsageReported                  time.Time
	providerLastUsage              map[string]time.Time
	ProviderUsageErr               map[string]string
	executionSyncFailures          map[string]int
	executionInventorySynced       map[string]int
	// sinkPending are the events of the sink's verbs within sink.Window,
	// sinking is set while their dispatch runs, sinkLedger claims each one
	// once across the machine's watches.
	sinkPending []state.Event
	sinking     atomic.Bool
	sinkLedger  *sink.Ledger
	// sweeping is set while the exposure sweep runs, lastSweep when it began.
	Sweeping  atomic.Bool
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
	LastClaudeLogin string
	// centraling is set while the roster is published to the central
	// instance, lastCentral when that last started and centralChecked when
	// the central instance was last called.
	centraling     atomic.Bool
	lastCentral    time.Time
	centralChecked time.Time
	// polls counts the polls begun.
	Polls atomic.Int64
	// failed counts the reads that failed (fail): --once exits non-zero on
	// any, so a silent run means no change.
	Failed        atomic.Int64
	ResolverState resolverWatch
	// missing are the sections whose platform part this build does not
	// have, said once each.
	missing map[string]bool
	// chores runs the doctor every poll (not for --once); doctoring is set
	// while its run goes, and retitled holds when it last retitled each
	// desktop session.
	Chores    bool
	Doctoring atomic.Bool
	Retitled  map[string]time.Time
	// goCacheOn trims the Go build cache (not for --once); goCaching is
	// set while a look at it goes, goCacheNext is when the next is due.
	GoCacheOn   bool
	goCaching   atomic.Bool
	GoCacheNext time.Time
	// dockerOn prunes Docker's images and build cache (not for --once);
	// dockerPruning is set while a look at it goes, dockerNext is when the
	// next is due.
	DockerOn      bool
	DockerPruning atomic.Bool
	DockerNext    time.Time
	// freeOn runs the automatic free of tmpfs (not for --once); freeing is
	// set while a run goes, freeNext is when the next is due and lastFree
	// the last run any watch did, for the TMPFS line.
	FreeOn   bool
	Freeing  atomic.Bool
	FreeNext time.Time
	LastFree atomic.Pointer[freeMark]
	// diskPruneOn enables the scheduled and pressure-triggered disk step.
	// diskPruning orders access to diskPruneNext across background steps.
	DiskPruneOn   bool
	DiskPruning   atomic.Bool
	DiskPruneNext time.Time
	// holdersOn lists the state home's holders (not for --once);
	// holdering orders access to holdersNext across background looks.
	HoldersOn   bool
	Holdering   atomic.Bool
	HoldersNext time.Time
	// worktreesOn trims finished workers' worktrees for the legacy ask mode.
	WorktreesOn       bool
	WorktreesTrimming atomic.Bool
	WorktreesNext     time.Time
	// slotsOn stops orphaned slot scopes (not for --once); orphanSince is
	// when the watch first saw each orphaned slot scope.
	SlotsOn          bool
	orphanSince      map[string]time.Time
	emptyHolderSince map[string]time.Time
	// doctored are the chore lines the doctor said, each said once.
	Doctored map[string]bool
	// timerActs are the fired timers' wakes and commands under way, and
	// the defaulted decisions' filers being told.
	TimerActs sync.WaitGroup
	// bg are the poll's background runs under way (inFlight, the doctor,
	// the Go cache's, Docker's and tmpfs's looks): a restart waits for them.
	Bg sync.WaitGroup
	// restart tells the watch when an install asked it to restart on the
	// installed binary (nil: never, --once); restartFor is the request
	// its run ended for.
	Restart    *restartWatch
	RestartFor *restartRequest
	// openNotes are the notes open at the last tick, answersSince the time
	// up to which their answers were said.
	openNotes    map[int]bool
	answersSince time.Time
	// replaced says whether a process's binary was replaced, and its path;
	// nil is platform.ProcessBinary. versionOf is the version a binary file
	// reports; nil runs it. gateOf runs budget --gate on an installed binary.
	Replaced    func(pid int) (string, bool)
	VersionOf   func(ctx context.Context, file string) string
	GateOf      func(ctx context.Context, file string) error
	gateChecked map[string]bool
}

// Unavailable reports whether err is a platform part this build does not
// have. The first time per section the watch says so in one line, never
// as a condition or an alert.
func (w *Watcher) Unavailable(section string, err error) bool {
	if !platform.Missing(err) {
		return false
	}
	w.Mu.Lock()
	said := w.missing[section]
	if w.missing == nil {
		w.missing = map[string]bool{}
	}
	w.missing[section] = true
	w.Mu.Unlock()
	if !said {
		w.EmitNow("unavailable", "%s", platform.Unavailable(section))
	}
	return true
}

// helmReleases reads the lane installation's HelmReleases.
func (w *Watcher) helmReleases(ctx context.Context, lane config.Lane) ([]merge.HelmRelease, error) {
	if w.ReadHRs != nil {
		return w.ReadHRs(ctx, lane)
	}
	return gate.ReadHelmReleases(ctx, lane)
}

// watchClock is a watch's time: its loops wait on its timers.
type watchClock interface {
	Now() time.Time
	// Timer fires once after d; stop releases it.
	Timer(d time.Duration) (fire <-chan time.Time, stop func())
}

// WallClock is the time a watch keeps outside its tests: beekeeper's clock,
// the system's in a release build and a scenario world's in a scenario.
type WallClock struct{}

func (WallClock) Now() time.Time { return clk.Now() }

func (WallClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	t := clk.NewTimer(d)
	return t.C, func() { t.Stop() }
}

func (w *Watcher) Clk() watchClock {
	if w.Clock != nil {
		return w.Clock
	}
	return WallClock{}
}

func (w *Watcher) Run(ctx context.Context, once bool) error {
	if once {
		w.BatchEvery = 0
	} else if w.BatchEvery == 0 {
		w.BatchEvery = w.Host.cfg.Watch.BatchInterval.Duration
	}
	w.lastPoll = w.Clk().Now().Add(-w.Host.cfg.Watch.Interval.Duration)
	s, err := plat.Machine.DesktopScope()
	if s != nil {
		w.scopeOOM = s.OOMKills
	}
	if !w.Unavailable(secScope, err) {
		w.Check("noscope", s == nil, "no Claude Desktop scope found; watching the machine numbers only")
	}
	interval := w.Host.cfg.Watch.Interval.Duration
	if once {
		w.Host.ctx = ctx
		w.UpgradeCycle(ctx)
		w.Sample(ctx)
		w.lastSweep = clk.Now() // swept here, not in the poll's background
		w.Exposures()
		w.lastKindSpend = w.Clk().Now() // summed here, not in the poll's background
		w.KindSpending(w.lastKindSpend)
		w.Poll(ctx)
		w.Stand.Inflight.Wait() // a successor's start outlives no watch
		w.TimerActs.Wait()      // nor a timer's wake or command
		return w.OnceErr()
	}
	w.DiskPruneOn = !w.LoopOwned
	w.GoCacheOn = true
	w.HoldersOn = true
	w.DockerOn = true
	w.FreeOn = true
	w.WorktreesOn = true
	w.SlotsOn = true
	w.ReadFreeMark()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	w.Host.ctx = ctx
	var wg sync.WaitGroup
	if w.Standby {
		w.guideRecovery = true
		g := w.NewGuideRecovery()
		wg.Go(func() {
			defer g.Stand.Inflight.Wait()
			g.Loop(ctx, min(interval, 30*time.Second), false, g.RecoverGuide)
		})
	}
	// The machine is sampled in a loop of its own, so no network read or
	// subprocess of the rest of the poll ever delays a memory or load line.
	// The first sample comes before the reads start: they begin knowing
	// whether the machine is strained. The loop's own first sample is one
	// interval later: two samples in a row span one (CPU PRESSURE).
	w.Sample(ctx)
	defer wg.Wait()
	if w.BatchEvery > 0 {
		wg.Go(func() { w.BatchLoop(ctx) })
	}
	wg.Go(func() {
		fire, stop := w.Clk().Timer(interval)
		defer stop()
		select {
		case <-ctx.Done():
		case <-fire:
			w.Loop(ctx, interval, false, w.Sample)
		}
	})
	if !w.Standby {
		wg.Go(func() { w.watchAlerts(ctx) })
		if w.Host.cfg.Alerts.PagerDuty.Enabled() {
			wg.Go(func() { w.watchPagerDuty(ctx) })
		}
		if w.Host.cfg.Alerts.Discover.Enabled() {
			wg.Go(func() { w.watchDiscovery(ctx) })
		}
	}
	if w.Host.cfg.Central.Enabled() {
		w.Host.hub() // made before the feed, the mailbox and the roster sync share it
		wg.Go(func() { w.WatchFeed(ctx) })
		if w.Standby {
			wg.Go(func() { w.WatchMailbox(ctx) })
		}
	}
	wg.Go(func() {
		w.Loop(ctx, interval, true, func(ctx context.Context) {
			if w.LoopOwned {
				if _, replaced := w.replacedBinary(os.Getpid()); replaced {
					return
				}
			}
			w.UpgradeCycle(ctx)
		})
	})
	w.Loop(ctx, interval, false, func(ctx context.Context) {
		if w.LoopOwned {
			if _, replaced := w.replacedBinary(os.Getpid()); replaced {
				return
			}
		}
		w.Poll(ctx)
		if req := w.restartDue(); req != nil {
			w.checkInstalledGate(ctx, req.Binary)
			w.RestartFor = req
			cancel(errRestart)
		}
	})
	// Every shutdown waits for the runs the watch started, including saves
	// still writing its state after cancellation.
	wg.Wait()
	w.Bg.Wait()
	w.Stand.Inflight.Wait()
	w.TimerActs.Wait()
	if w.RestartFor == nil {
		return nil
	}
	// A restarted watch's mark says nothing twice.
	w.SaveMark()
	return errRestart
}

// restartDue is the install's request this watch restarts for, nil when
// none.
func (w *Watcher) restartDue() *restartRequest {
	if w.Restart == nil {
		return nil
	}
	return w.Restart.Due()
}

// SlowReads is how many times less often the installation reads run while
// the machine is strained.
const SlowReads = 4

// Loop runs fn every interval until ctx ends. A run that overruns skips the
// ticks it missed instead of queueing them. An installation read (reads)
// runs every slowReads × interval, at nice 10, while the machine is
// strained.
func (w *Watcher) Loop(ctx context.Context, interval time.Duration, reads bool, fn func(context.Context)) {
	own := w.Clk()
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
func (w *Watcher) readEvery(interval time.Duration) time.Duration {
	if w.strained.Load() {
		return SlowReads * interval
	}
	return interval
}

// readCtx runs an installation read's commands at nice 10 while the machine
// is strained.
func (w *Watcher) readCtx(ctx context.Context) context.Context {
	if w.strained.Load() {
		return proc.Background(ctx)
	}
	return ctx
}

// InFlight runs fn unless its previous run (running) still goes, and waits
// for it at most wait: a hung read holds the poll up for one wait at most,
// and the next poll skips it instead of starting a second one. fn keeps
// running on ctx, within its own timeouts.
func (w *Watcher) InFlight(ctx context.Context, wait time.Duration, running *atomic.Bool, fn func(context.Context)) {
	if ctx.Err() != nil || !running.CompareAndSwap(false, true) {
		return
	}
	done := make(chan struct{})
	w.Bg.Add(1)
	go func() {
		defer w.Bg.Done()
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
func (w *Watcher) watchAlerts(ctx context.Context) {
	store := alerts.NewStore(w.Host.cfg.StateDir)
	defer func() { _ = store.Release() }()
	// One port-forward per installation, kept from tick to tick: a tick
	// is then HTTP requests, not kubectl and tsh processes.
	reader := w.Host.alertReader()
	reader.Keep = true
	defer reader.Close()
	other := 0
	w.Loop(ctx, w.Host.cfg.Alerts.Every.Duration, true, func(ctx context.Context) {
		owned, owner, err := store.Own()
		switch {
		case err != nil:
			w.Emit("alerts", "ALERTS baseline unusable: %v", err)
		case !owned:
			if base, err := store.Load(); err == nil {
				w.PublishAlerts(base.Firing(""))
			}
			reader.Close()
			w.Clear("alerts")
			if owner.PID != other {
				other = owner.PID
				w.EmitNow("alerts", "ALERTS read by the watch with pid %d; this one takes over when it ends", other)
			}
		default:
			w.Clear("alerts")
			if other != 0 {
				other = 0
				w.EmitNow("alerts", "ALERTS taken over by this watch")
			}
			lines, noContext := w.Host.alertCycle(ctx, store, reader)
			if base, err := store.Load(); err == nil {
				w.PublishAlerts(base.Firing(""))
			}
			for _, l := range lines {
				w.EmitNow("alerts", "%s", l)
			}
			w.sayNoContext("ALERTS", noContext)
		}
	})
}

// Emit says a lasting condition once, when it starts. While it lasts it is
// silent and returns its line at most every watch.repeat, for the
// notification a person gets (notify.repeat decides whether it goes out).
func (w *Watcher) Emit(key, format string, args ...any) string {
	line := fmt.Sprintf(format, args...)
	w.Mu.Lock()
	now := clk.Now()
	if w.Active == nil {
		w.Active = map[string]Condition{}
	}
	_, active := w.Active[key]
	if active && now.Sub(w.Last[key]) < w.Host.cfg.Watch.Repeat.Duration {
		w.Mu.Unlock()
		return ""
	}
	w.Last[key] = now
	if !active {
		label, _, _ := strings.Cut(line, ":")
		w.Active[key] = Condition{Since: now, Label: label}
		w.Dirty = true
	}
	w.Mu.Unlock()
	if !active {
		w.EmitNow(key, "%s", line)
	}
	return line
}

// OnceErr is --once's outcome: an error once a read failed, its lines said
// already.
func (w *Watcher) OnceErr() error {
	if n := w.Failed.Load(); n > 0 {
		return fmt.Errorf("watch --once: %d reads failed, said above", n)
	}
	return nil
}

// Fail says a read that failed as a condition (emit) and counts it: --once
// exits non-zero on any.
func (w *Watcher) Fail(key, format string, args ...any) string {
	w.Failed.Add(1)
	return w.Emit(key, format, args...)
}

// Check says a condition's start (emit) while on holds and its end, one
// ENDED line, once it no longer does.
func (w *Watcher) Check(key string, on bool, format string, args ...any) string {
	if on {
		return w.Emit(key, format, args...)
	}
	w.Clear(key)
	return ""
}

// Clear ends a condition the watch has said: one ENDED line.
func (w *Watcher) Clear(key string) {
	w.Mu.Lock()
	c, active := w.Active[key]
	delete(w.Active, key)
	w.Dirty = w.Dirty || active
	w.Mu.Unlock()
	if active {
		if w.Host.store != nil {
			_ = w.Host.store.Record(event(WatchParty, "watch.clear", "%s", key))
		}
		w.EmitNow("ended", "ENDED %s (since %s)", c.Label, c.Since.Local().Format("15:04"))
	}
}

// IsActive reports whether the watch has said the condition key and not yet
// its end.
func (w *Watcher) IsActive(key string) bool {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	_, active := w.Active[key]
	return active
}

// ClearMissing ends each condition said under prefix that found no longer
// holds: one ENDED line each.
func (w *Watcher) ClearMissing(prefix string, found map[string]bool) {
	w.Mu.Lock()
	var gone []string
	for k := range w.Active {
		if strings.HasPrefix(k, prefix) && !found[k] {
			gone = append(gone, k)
		}
	}
	w.Mu.Unlock()
	for _, k := range gone {
		w.Clear(k)
	}
}

// drop forgets each condition said under prefix that found no longer holds,
// without an ENDED line, and answers them.
func (w *Watcher) drop(prefix string, found map[string]bool) map[string]Condition {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	gone := map[string]Condition{}
	for k, c := range w.Active {
		if strings.HasPrefix(k, prefix) && !found[k] {
			gone[k] = c
			delete(w.Active, k)
			w.Dirty = true
		}
	}
	return gone
}

// Condition is a lasting condition a watch has said: since when, and the
// line's head that names it.
type Condition struct {
	Since time.Time `json:"since"`
	Label string    `json:"label"`
}

// WatchMark is what a caller's watch has said and a restarted watch of the
// same caller must not say again: the conditions open (it says only their
// end) and the one-time events (runaways, stale leases).
type WatchMark struct {
	Conditions    map[string]Condition `json:"conditions"`
	Reported      map[string]bool      `json:"reported"`
	FeedID        string               `json:"feedID,omitempty"`
	UsageReported time.Time            `json:"usageReported,omitzero"`
}

// SaveMark keeps what the watch has said for its successor, when it changed.
func (w *Watcher) SaveMark() {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	if w.MarkFile == "" || !w.Dirty {
		return
	}
	w.Dirty = false
	_ = w.Host.store.WriteFile(w.MarkFile, WatchMark{Conditions: w.Active, Reported: w.Reported, FeedID: w.FeedID, UsageReported: w.UsageReported})
}

// Notify sends one event that needs a person (--notify): a lasting kind
// takes no key.
func (w *Watcher) Notify(ctx context.Context, kind, key, summary, body string) {
	w.NotifyAt(ctx, w.Host.now, kind, key, summary, body)
}

// NotifyAt is notify for a loop of its own, which keeps its own time.
func (w *Watcher) NotifyAt(ctx context.Context, now time.Time, kind, key, summary, body string) {
	if w.Notifier != nil && body != "" {
		w.Notifier.Notify(ctx, now, kind, key, summary, body)
	}
}

// swapSample is one poll's swap, zswap's writeback since boot and
// MemAvailable.
type swapSample struct {
	at                                         time.Time
	usedMiB, diskMiB, writtenBackMiB, availMiB int
}

// SwapTrend is the swap's movement over watch.swapWindow: disk swap's
// growth from new swap-outs, zswap's writeback to disk, the growth of all
// swap in use (what systemd-oomd measures), and whether MemAvailable fell.
// Rated once the readings span config.MinSwapSpan.
type SwapTrend struct {
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

// Pressing reports disk swap growing while MemAvailable falls: the only
// swap reading that means memory pressure. Swapped pages stay in swap long
// after the burst that pushed them out, and zswap's share sits in RAM.
func (t SwapTrend) Pressing() bool { return t.Rated && t.DiskPerHourMiB > 0 && t.AvailFalling }

// SwapTrend records a reading and returns the movement over
// watch.swapWindow.
func (w *Watcher) SwapTrend(now time.Time, m machine.Mem) SwapTrend {
	window := w.Host.cfg.Watch.SwapWindow.Duration
	w.SwapSamples = append(w.SwapSamples, swapSample{now, m.SwapUsedMiB, m.DiskSwapMiB(), m.ZswapWrittenBackMiB, m.AvailableMiB})
	i := 0
	for i < len(w.SwapSamples)-1 && now.Sub(w.SwapSamples[i].at) > window {
		i++
	}
	w.SwapSamples = w.SwapSamples[i:]
	first := w.SwapSamples[0]
	span := now.Sub(first.at)
	if span < config.MinSwapSpan {
		return SwapTrend{Window: window}
	}
	perHour := func(mib int) int { return int(float64(mib) / span.Hours()) }
	writtenBack := max(0, m.ZswapWrittenBackMiB-first.writtenBackMiB)
	return SwapTrend{
		DiskPerHourMiB:      perHour(m.DiskSwapMiB() - first.diskMiB - writtenBack),
		WritebackPerHourMiB: perHour(writtenBack),
		UsedPerHourMiB:      perHour(m.SwapUsedMiB - first.usedMiB),
		AvailFalling:        m.AvailableMiB < first.availMiB,
		Rated:               true,
		Window:              window,
	}
}

// SwapLine says swap in use as disk plus zswap, systemd-oomd's swap rule
// (its trigger only when it watches a cgroup for swap), disk swap's growth
// from new swap-outs and zswap's writeback apart: the numbers that decide
// whether swap means pressure.
func SwapLine(m machine.Mem, oomd *machine.OOMDSwap, t SwapTrend) string {
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
	if t.Pressing() {
		line += " while MemAvailable falls"
	}
	if oomd != nil && oomd.Watched() && t.UsedPerHourMiB > 0 && oomd.HeadroomMiB(m) > 0 {
		line += ", trigger in " + untilTrigger(oomd.HeadroomMiB(m), t.UsedPerHourMiB).Round(time.Minute).String()
	}
	return line
}

// SwapOver reports disk swap over maxMiB and pressing; zswap's share,
// held in RAM, never counts.
func SwapOver(m machine.Mem, t SwapTrend, maxMiB int) bool {
	return m.DiskSwapMiB() > maxMiB && t.Pressing()
}

// untilTrigger is how long headroom lasts at perHour.
func untilTrigger(headroom, perHour int) time.Duration {
	return time.Duration(float64(headroom) / float64(perHour) * float64(time.Hour))
}

// OomdImminent reports whether systemd-oomd's swap kill is near enough to
// need a person: oomd watches a cgroup for swap, disk swap grows while
// MemAvailable falls, and the headroom is under watch.oomdHeadroomMinMiB
// (a fraction of SwapTotal by default) or the trigger within
// watch.oomdWithin at the growth of the swap in use.
func (w *Watcher) OomdImminent(m machine.Mem, oomd *machine.OOMDSwap, t SwapTrend) bool {
	if oomd == nil || !oomd.Watched() || !t.Pressing() {
		return false
	}
	th := w.Host.cfg.Watch
	headroom := oomd.HeadroomMiB(m)
	if headroom < th.OOMDHeadroomMin(m.SwapTotalMiB) {
		return true
	}
	return t.UsedPerHourMiB > 0 && untilTrigger(headroom, t.UsedPerHourMiB) < th.OOMDWithin.Duration
}

// modelServer says the host models no model-server lease covers and
// unloads those a lab loaded or a budget exceeds, unless ollama.nameOnly.
func (w *Watcher) modelServer(ctx context.Context, models []machine.HostModel) {
	holders, err := lease.Dir(w.Host.cfg.LeaseDir).List()
	if err != nil {
		return
	}
	bs := ModelBreaches(models, holders, w.Host.cfg.Resources, w.Host.cfg.Ollama.BudgetGiB)
	unloaded := map[string]error{}
	for _, b := range bs {
		if b.Unload && !w.Host.cfg.Ollama.NameOnly {
			unloaded[b.Model.Name] = b.Model.Unload(ctx, ServerURL(w.Host.cfg, b.Model))
		}
	}
	w.Check("modelserver", len(bs) > 0, "MODEL SERVER: %s", ModelServerLine(bs, unloaded))
}

// EmitNow records every event immediately, then batches only known routine lines.
func (w *Watcher) EmitNow(key string, format string, args ...any) {
	w.emitLinesNow(key, []string{fmt.Sprintf(format, args...)})
}

func (w *Watcher) emitLinesNow(key string, lines []string) {
	var evs []state.Event
	var kept []string
	for _, line := range lines {
		if key != "resolver" && w.resolverFailure(line) {
			continue
		}
		kept = append(kept, line)
		evs = append(evs, event(WatchParty, "watch."+key, "%s", line))
	}
	if w.Host.store != nil && len(evs) > 0 {
		_ = w.Host.store.Record(evs...)
	}
	for _, line := range kept {
		w.OutputEvent(line)
	}
}

// Sample reads the machine's numbers (memory, swap, load, pressure, disk,
// the desktop scope) from /proc and /sys, and decides whether the machine
// is strained. It keeps its own time: it runs in a loop of its own.
func (w *Watcher) Sample(ctx context.Context) {
	now := clk.Now()
	if st, err := w.Host.readSnapshot(); err == nil {
		w.SayAppDrifts(w.Host.appDrifts(ctx, st.Apps))
	}
	th := w.Host.cfg.Watch

	// GTT is RAM the iGPU pins outside every cgroup: above its threshold
	// it is named as the cause of low memory, with the model servers' models.
	models, merr := hostModels(ctx, w.Host.cfg)
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
	w.Unavailable(secMemory, merr)
	var cause string
	if gpus := machine.ReadGPUs(); machine.GTTUsedMiB(gpus) > th.GTTMax(m.TotalMiB) {
		cause = gttLine(gpus, models)
	}
	w.Check("gtt", cause != "", "IGPU %s", strings.TrimPrefix(cause, "iGPU "))
	if merr == nil {
		w.Check("avail", m.AvailableMiB < th.AvailMin(m.TotalMiB), "LOW RAM: %d MiB available, swap %s%s", m.AvailableMiB, m.SwapSplit(), Because(cause))
		var oomd *machine.OOMDSwap
		if o, err := plat.Machine.OOMDSwap(ctx); err == nil {
			oomd = &o
		}
		t := w.SwapTrend(now, m)
		line := SwapLine(m, oomd, t) + Because(cause)
		w.Check("swap", SwapOver(m, t, th.SwapMax(m.SwapTotalMiB)), "%s", line)
		// A running swapoff shrinks SwapTotal ahead of the pages it drains:
		// swap reads full while it empties, and oomd is no nearer.
		swapoff := SwapoffRuns(table)
		w.Check("swapoff", swapoff, "SWAPOFF IN PROGRESS: %s", line)
		if m.SwapTotalMiB > 0 {
			w.Check("oomd", !swapoff && w.OomdImminent(m, oomd, t), "OOMD IMMINENT: %s", line)
		}
		w.KeepSwap(&capacity.SwapReading{At: now, UsedMiB: m.SwapUsedMiB, DiskMiB: m.DiskSwapMiB(), ZswapMiB: m.ZswappedMiB,
			PerHourMiB: t.DiskPerHourMiB, WritebackPerHourMiB: t.WritebackPerHourMiB, Rated: t.Rated})
	}
	w.SampleCPU(now, table)
	psi, err := plat.Machine.MemoryPressure()
	w.Unavailable(secPressure, err)
	if err == nil {
		w.Check("psi", psi > th.PSIMax, "MEMORY PRESSURE: full avg60 %.0f%%", psi)
	}
	if d, err := machine.ReadDisk("/tmp"); err == nil {
		large := d.UsedMiB > th.TmpMax(d.TotalMiB)
		var largest string
		if large {
			var owners map[int]string
			if o := w.Owners.Load(); o != nil {
				owners = *o
			}
			largest = "; " + TmpfsLargest("/tmp", table, owners, proc.References)
		}
		w.Check("tmp", large, "TMPFS /tmp: %d MiB; %s%s", d.UsedMiB, w.LastFreeNote(), largest)
	}
	if d, err := machine.ReadDisk("/"); err == nil {
		var owners map[int]string
		if o := w.Owners.Load(); o != nil {
			owners = *o
		}
		w.SampleDisk(now, d, w.cpuTable, owners)
	}
	w.GoCache(now)
	w.docker(now)
	w.autoFree(now)
	w.worktrees(now)
	w.DiskPrune(now, false)
	w.StateHolders(now)
	w.SlotOrphans(now, table)
	s, err := plat.Machine.DesktopScope()
	w.Unavailable(secScope, err)
	if s != nil {
		w.Check("scopeanon", s.AnonMiB > th.ScopeAnonMax(m.TotalMiB), "DESKTOP SCOPE anon: %d MiB", s.AnonMiB)
		if s.OOMKills != w.scopeOOM {
			w.EmitNow("scopeoom", "OOM KILL in the desktop scope: oom_kill %d -> %d", w.scopeOOM, s.OOMKills)
			w.scopeOOM = s.OOMKills
		}
	}
	w.SaveMark()
	if w.LoopOwned {
		w.PublishMachine()
	}
}

// SampleCPU says the CPU lines: HIGH LOAD over watch.loadMax (or
// watch.loadPerCoreMax × cores), LOAD RISING on a steep climb, CPU PRESSURE
// once two samples in a row read some avg10 over watch.cpuPSIMax, each
// with the top CPU consumers since the last sample; and READS SLOWED while
// the machine is strained.
func (w *Watcher) SampleCPU(now time.Time, t *proc.Table) {
	th := w.Host.cfg.Watch
	cores := runtime.NumCPU()
	limit := th.LoadLimit(cores)
	load, err := plat.Machine.Load()
	w.Unavailable(secLoad, err)
	cpu, err := plat.Machine.CPUPressure()
	w.Unavailable(secPressure, err)
	if cpu > th.CPUPSIMax {
		w.cpuOver++
	} else {
		w.cpuOver = 0
	}
	var top string
	if t != nil {
		span := now.Sub(w.cpuAt)
		top = TopCPULine(TopCPU(w.cpuTable, t, span, topCPUCommands), span)
		w.SampleProcs(now, span, w.cpuTable, t)
		w.cpuTable, w.cpuAt = t, now
	}
	w.Check("load", load[0] > limit, "HIGH LOAD: 1m %.0f over %.0f (%d cores)%s", load[0], limit, cores, top)
	w.Check("loadrising", LoadRising(load, cores), "LOAD RISING: 1m %.0f, 5m %.0f (%d cores)%s", load[0], load[1], cores, top)
	// A restarted watch keeps a CPU PRESSURE it said while it lasts.
	said := w.IsActive("cpupsi")
	w.Check("cpupsi", w.cpuOver >= 2 || said && w.cpuOver > 0, "CPU PRESSURE: some avg10 %.0f%% over %.0f%%%s", cpu, th.CPUPSIMax, top)
	strained := load[0] > limit || cpu > th.CPUPSIMax
	w.strained.Store(strained)
	w.Check("slowed", strained, "READS SLOWED: machine under CPU pressure (load %.0f, CPU some avg10 %.0f%%): installation reads every %d× their interval, at nice %s",
		load[0], cpu, SlowReads, proc.Niceness)
}

// SwapoffRuns reports whether a swapoff runs.
func SwapoffRuns(t *proc.Table) bool {
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

// WatchOwner names the watch's own processes in the PROCESS STORM, LOAD and
// STACKED lines.
const WatchOwner = "beekeeper watch"

// SampleProcs says PROCESS STORM once two samples in a row read a fork rate
// more than watch.forkRateMax over the machine's usual one, LOAD while more
// than watch.toolProcsMax processes of watch.tools run, and a STACKED line
// for each command line running more than watch.stackMax times over, each
// with an ENDED line when it ends.
// prev is the process table the last sample read, span ago.
func (w *Watcher) SampleProcs(now time.Time, span time.Duration, prev, t *proc.Table) {
	th := w.Host.cfg.Watch
	owners := map[int]string{}
	if o := w.Owners.Load(); o != nil {
		maps.Copy(owners, *o)
	}
	// The watch's own processes (its alert reads among them) are the
	// watch's, not the session's that started it.
	owners[cmp.Or(w.Self, os.Getpid())] = WatchOwner
	read := w.ReadForks
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
		said := w.IsActive("forks")
		if w.forkOver >= 2 || said && w.forkOver > 0 {
			w.Emit("forks", "%s", StormLine(rate, w.forkUsual, Fresh(prev, t), t, owners))
		} else {
			w.Clear("forks")
		}
	}
	if err == nil {
		w.forks = forks
	}

	var load string
	if th.ToolProcsMax >= 0 {
		load = ToolLoadLine(t, th.Tools, th.ToolProcsMax, owners)
	}
	w.Check("toolprocs", load != "", "%s", load)

	var found []stack
	if th.StackMax > 0 {
		found = Stacks(t, now, th.StackMax, owners)
	}
	cur := map[string]bool{}
	for _, s := range found {
		key := "stacked " + s.Key
		cur[key] = true
		w.Emit(key, "%s", s.Line())
	}
	// The STACKED lines said, a restarted watch's among them.
	w.Mu.Lock()
	var gone []string
	for key := range w.Active {
		if strings.HasPrefix(key, "stacked ") && !cur[key] {
			gone = append(gone, key)
		}
	}
	w.Mu.Unlock()
	for _, key := range gone {
		w.Clear(key)
	}
}

// PollSessions does what the process table and the sessions in it tell.
func (w *Watcher) PollSessions(ctx context.Context, since time.Time, t *proc.Table) {
	if ctx.Err() != nil {
		return
	}
	w.Table = t
	sessions := discover(w.Host.cfg, t, w.Host.now)
	owners := make(map[int]string, len(sessions))
	for _, s := range sessions {
		owners[s.PID] = s.Name
	}
	w.Owners.Store(&owners)
	w.Suspends(ctx, since, sessions)
	w.kills(ctx, since, sessions, t)
	w.TendReporter(ctx, sessions)
	w.tendFeedback(ctx)
	if ctx.Err() != nil {
		return
	}
	w.Pending(ctx, sessions)
	if ctx.Err() != nil {
		return
	}
	w.SessionChanges(sessions)
	w.StaleLeases(ctx, sessions)
	w.UnownedPages(ctx, sessions)
	w.Runaways(sessions, t)
	w.Twins(sessions, t)
	w.Ungated(sessions, t)
	if ctx.Err() != nil {
		return
	}
	w.SameTitles(w.StopRevived(ctx, sessions))
	if ctx.Err() != nil {
		return
	}
	w.StaleWatches(ctx, t)
}

// twinKey starts the condition key of a session that runs two CLIs, and
// twinGrace is how long the youngest of them runs before they count.
const (
	twinKey   = "twin "
	twinGrace = 30 * time.Second
)

// Twins says each session that runs more than one Claude Code CLI on its
// session id (a headless turn beside its desktop CLI): both act on its task,
// each unaware of the other, and a message by name reaches only one. One
// TWIN CLI line naming every CLI's PID and directory, and its ENDED line once
// one CLI is left. Discover keeps one session per id, so the CLIs are counted
// in the process table; a restarted CLI overlaps its predecessor for a
// moment, so CLIs count once the youngest has run for twinGrace.
func (w *Watcher) Twins(sessions []*claude.Session, t *proc.Table) {
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
		if len(cli) < 2 || slices.ContainsFunc(cli, func(p *proc.Process) bool { return w.Host.now.Sub(p.Start) < twinGrace }) {
			continue
		}
		slices.SortFunc(cli, func(a, b *proc.Process) int { return cmp.Compare(a.PID, b.PID) })
		var each []string
		for _, p := range cli {
			each = append(each, fmt.Sprintf("PID %d in %s", p.PID, t.Cwd(p.PID)))
		}
		key := twinKey + id
		found[key] = true
		w.Emit(key, "TWIN CLI: %q runs %d CLIs on session %s (%s): both act on its task and a message by name reaches only one; stop the one that should not run",
			names[id], len(cli), id, strings.Join(each, ", "))
	}
	w.ClearMissing(twinKey, found)
}

// Poll does everything but the machine sample: the process table, the
// sessions, the lanes and the budget. It waits for a lane read or a budget
// probe for one watch.interval at most.
func (w *Watcher) Poll(ctx context.Context) {
	w.Polls.Add(1)
	w.Host.now = w.Clk().Now()
	since := w.lastPoll
	w.lastPoll = w.Host.now
	th := w.Host.cfg.Watch

	t, err := plat.Machine.Processes()
	switch {
	case w.Unavailable(secSessions, err):
		// No session is known: what reads them is left out, not guessed.
	case err != nil:
		w.Fail("proc", "cannot read the process table: %v", err)
		return
	default:
		w.Clear("proc")
		w.PollSessions(ctx, since, t)
	}
	if ctx.Err() != nil {
		return
	}
	w.StaleMerges()
	w.LostMerges(ctx)
	w.OwedOutcomes(ctx)
	w.Host.pollToolWindow(ctx, WatchParty)
	w.Stalls()
	w.SlotHolds()
	w.Unbrokered()
	w.VaultWaits()
	if ctx.Err() != nil {
		return
	}
	w.Sink(ctx)
	now := w.Host.now
	if now.Sub(w.lastSettle) >= w.readEvery(th.Interval.Duration) {
		w.lastSettle = now
		w.InFlight(w.readCtx(ctx), th.Interval.Duration, &w.settling, func(ctx context.Context) {
			_ = w.Host.checkPlaces(ctx, WatchParty, now, "", "", 0, th.Interval.Duration)
			w.HungMerges(ctx, now)
			w.Settled(ctx, now)
		})
	}
	if now.Sub(w.LastBudget) >= th.BudgetEvery.Duration {
		w.LastBudget = now
		w.InFlight(ctx, th.Interval.Duration, &w.budgeting, func(ctx context.Context) { w.Budget(ctx, now) })
	}
	if now.Sub(w.lastKindSpend) >= w.readEvery(kindSpendEvery) {
		w.lastKindSpend = now
		// the day's transcripts are read whole: the poll does not wait
		w.InFlight(ctx, 0, &w.kindSumming, func(context.Context) { w.KindSpending(now) })
	}
	if now.Sub(w.lastTeleport) >= w.readEvery(th.Interval.Duration) {
		w.lastTeleport = now
		w.ClaudeLogin()
		w.InFlight(ctx, th.Interval.Duration, &w.teleporting, w.teleport)
	}
	if now.Sub(w.lastServing) >= w.readEvery(th.Interval.Duration) {
		w.lastServing = now
		// A drain of a self-hosted model waits for its sessions: the watch
		// runs the switch the constraint asked for once they are gone.
		w.InFlight(ctx, th.Interval.Duration, &w.serving, func(ctx context.Context) {
			_ = w.Host.reconcileServeModes(ctx)
		})
	}
	if w.Host.cfg.Central.Enabled() && now.Sub(w.lastCentral) >= w.readEvery(th.Interval.Duration) {
		w.lastCentral = now
		w.InFlight(ctx, th.Interval.Duration, &w.centraling, w.SyncRoster)
	}
	if now.Sub(w.lastSweep) >= w.readEvery(w.Host.cfg.Outbound.SweepEvery.Duration) {
		w.lastSweep = now
		// The sweep takes a while on a big home directory: the poll does not wait.
		w.InFlight(ctx, 0, &w.Sweeping, func(context.Context) { w.Exposures() })
	}
	if ctx.Err() != nil {
		return
	}
	w.SaveMark()
	if w.Notifier != nil {
		w.Notifier.Flush(ctx, w.Host.now)
	}
}

// teleport runs in the command, which holds what it needs (watchTeleport).
func (w *Watcher) teleport(ctx context.Context) {
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

// VaultWaits says the vault's state (secret.session): one VAULT UNLOCKED
// line while the broker holds the session, until when, and its ENDED line
// when the broker forgets it; a VAULT SIGN-IN RETRYING line while the
// broker waits for the credential store or tries a failed sign-in again,
// and a VAULT SIGN-IN FAILED line with the broker's reason once it gave
// up; a VAULT SESSION DROPPED line while a session op stopped taking
// waits for its new sign-in; one VAULT LOCKED line for each call that waits on the sign-in,
// and once it goes on its ENDED line, or a line that it timed out with the
// vault still locked. Nothing asks the person.
func (w *Watcher) VaultWaits() {
	if !w.Host.cfg.Secret.Session {
		return
	}
	statePath, err := secret.StatePath()
	if err != nil {
		return
	}
	st, err := secret.ReadState(statePath)
	if err != nil {
		w.Fail(vaultReadKey, "cannot read the vault's state: %v", err)
		return
	}
	w.Check(vaultUnlockedKey, st.Unlocked, "VAULT UNLOCKED: the broker holds the vault session since %s until %s",
		st.Since.Local().Format("15:04"), st.Until.Local().Format("15:04"))
	w.Check(vaultRetryKey, st.Retrying != "", "VAULT SIGN-IN RETRYING: %s", st.Retrying)
	w.Check(vaultSigninKey, st.Error != "", "VAULT SIGN-IN FAILED: %s", st.Error)
	w.Check(vaultDroppedKey, st.Dropped != "", "VAULT SESSION DROPPED at %s: op no longer took it (%s); the broker signs in again",
		st.DroppedAt.Local().Format("15:04"), st.Dropped)
	waitsPath, err := secret.WaitsPath()
	if err != nil {
		return
	}
	ws, err := secret.ReadWaits(waitsPath)
	if err != nil {
		w.Fail(vaultReadKey, "cannot read the vault's waiting calls: %v", err)
		return
	}
	w.Clear(vaultReadKey)
	found := map[string]bool{}
	for _, v := range ws {
		key := vaultKey + v.Who + " " + v.Ref
		found[key] = true
		w.Emit(key, "VAULT LOCKED: %s waits on %s since %s; the broker signs in",
			v.Who, v.Ref, v.Since.Local().Format("15:04"))
	}
	if !st.Unlocked {
		// a call gone while the vault is locked gave up: no ENDED line
		for key, c := range w.drop(vaultKey, found) {
			who, ref := key[len(vaultKey):], ""
			if i := strings.LastIndexByte(who, ' '); i >= 0 {
				who, ref = who[:i], who[i+1:]
			}
			w.EmitNow("vault", "VAULT LOCKED: %s's call on %s timed out (seen waiting since %s), still locked",
				who, ref, c.Since.Local().Format("15:04"))
		}
	}
	w.ClearMissing(vaultKey, found)
}

// exposedKey starts the condition key of a credential exposed on disk.
const exposedKey = "exposed "

// Exposures says each credential left exposed on disk (guard.Sweep), one
// EXPOSED line naming the file when it is found, never its content, and one
// ENDED line once it is fixed.
func (w *Watcher) Exposures() {
	ctx := w.Host.context()
	o := w.Host.cfg.Outbound
	found := map[string]bool{}
	for _, e := range guard.SweepContext(ctx, o.SweepRoots, o.SweepDepth) {
		key := exposedKey + e.Path
		found[key] = true
		w.Emit(key, "EXPOSED %s: %s", e.Path, e.What)
	}
	if ctx.Err() != nil {
		return
	}
	w.ClearMissing(exposedKey, found)
}

// Budget runs in the command, which holds what it needs (watchBudget).
func (w *Watcher) Budget(ctx context.Context, now time.Time) {
	watchBudget(w, ctx, now)
}

// CallersText runs in the command, which holds what it needs (watchCallersText).
func (w *Watcher) CallersText() string {
	return watchCallersText(w)
}

// Stalls runs in the command, which holds what it needs (watchStalls).
func (w *Watcher) Stalls() {
	watchStalls(w)
}

// LostMerges records the outcome of each running merge whose gate process
// and devctl are gone (recordGone), one MERGE CUT, MERGE RECORDED or MERGE
// LOST line each; a cut run's owed outcome wakes its owner in the same poll
// (owedOutcomes). The gate itself prunes a lost merge only when the lane's
// next merge arrives, and until then the lane shows a merge running that no
// process runs.
func (w *Watcher) LostMerges(ctx context.Context) {
	recorded, cut, lost := w.Host.recordGone(ctx)
	for _, d := range cut {
		w.EmitNow("lanes", "MERGE CUT: %s", d)
	}
	for _, d := range recorded {
		w.EmitNow("lanes", "MERGE RECORDED: %s", d)
	}
	for _, m := range lost {
		w.EmitNow("lanes", "MERGE LOST: %s in lane %s by %q: its gate (pid %d) and devctl are gone, whether it merged is unknown; the lane settles until %s, then frees once its HelmReleases are Ready",
			m.Key(), m.Lane, m.By.Name, m.PID, clock(w.Host.now, w.Host.now.Add(w.Host.cfg.Merge.Settle.Duration)))
	}
}

// HungMerges runs in the command, which holds what it needs (watchHungMerges).
func (w *Watcher) HungMerges(ctx context.Context, now time.Time) {
	watchHungMerges(w, ctx, now)
}

// Settled drops the settling merges whose lane has settled, so lanes shows
// the lane free before its next merge starts: a lane with no installation
// has nothing to roll (a merge whose release is unknown settles it by the
// settle rule), and one whose release rolled and whose HelmReleases
// are Ready is done, however late. A merge not settled past
// merge.settleTimeout is one LANE STUCK line with what the lane waits for,
// and one ENDED line once it settles or the lane is cleared.
func (w *Watcher) Settled(ctx context.Context, now time.Time) {
	st, err := w.Host.readSnapshot()
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
		lane, ok := w.Host.cfg.LaneNamed(m.Lane)
		if !ok || lane.Installation == "" {
			// A release its gate confirms settles with the verdict, one
			// unknown by the settle rule: it may be cutting.
			if merge.Confirming(m, proc.Alive) {
				continue
			}
			if ready, _ := merge.Ready(lane, nil, &m, now, w.Host.cfg.Merge.Settle.Duration); ready {
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
		} else if ready, wait := merge.Ready(lane, r.hrs, &m, now, w.Host.cfg.Merge.Settle.Duration); ready {
			done[m.Key()] = "rolled, HelmReleases of " + lane.Installation + " Ready"
			if wait != "" {
				done[m.Key()] = "settled without a roll, " + wait
			}
			continue
		} else {
			why = wait
		}
		if since := now.Sub(m.Finished); since > w.Host.cfg.Merge.SettleTimeout.Duration {
			key := "stuck:" + lane.Name + ":" + m.Key()
			stuck[key] = true
			if m.Release == "" && !m.ReleaseFailed {
				why += fmt.Sprintf("; recover with devctl release wait %s --pr %d, then name the release with beekeeper lanes settle %s %d --release <version>", m.Repo, m.PR, m.Repo, m.PR)
			}
			w.Emit(key, "LANE STUCK %s: %s has not settled %s after its merge: %s; fix the installation or clear the lane (beekeeper lanes clear %s)",
				lane.Name, m.Key(), dur(since), why, lane.Name)
		}
	}
	w.ClearMissing("stuck:", stuck)
	if len(done) == 0 {
		return
	}
	var settled []state.Merge
	_ = w.Host.store.Update(func(st *state.State) ([]state.Event, error) {
		var ev []state.Event
		st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool {
			why, ok := done[m.Key()]
			if !ok || m.Phase != state.Settling {
				return false
			}
			settled = append(settled, m)
			ev = append(ev, event(WatchParty, verbLaneSettled, "%s: %s %s", m.Lane, m.Key(), why))
			return true
		})
		return ev, nil
	})
	for _, m := range settled {
		w.Host.leaveCentral(ctx, []state.Merge{m}, done[m.Key()])
	}
}

// kills runs in the command, which holds what it needs (watchKills).
func (w *Watcher) kills(ctx context.Context, since time.Time, sessions []*claude.Session, t *proc.Table) {
	watchKills(w, ctx, since, sessions, t)
}

func (w *Watcher) SessionChanges(sessions []*claude.Session) {
	cur := map[string]*claude.Session{}
	for _, s := range sessions {
		cur[s.Key()] = s
	}
	if w.Sessions == nil {
		w.Sessions = cur
		return
	}
	var started, ended, restarted, quiet []string
	for k, s := range cur {
		prev, ok := w.Sessions[k]
		switch {
		case !ok:
			started = append(started, s.Name)
		case prev.PID != s.PID:
			restarted = append(restarted, s.Name)
		}
	}
	for k, s := range w.Sessions {
		_, ok := cur[k]
		recorded := slices.ContainsFunc(w.Records, func(r state.Record) bool { return r.Session.Is(s.Party()) })
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
			w.EmitNow("sessions", "SESSIONS %s: %s", c.what, quoted(loud))
		}
		if len(held) > 0 {
			quiet = append(quiet, fmt.Sprintf("SESSIONS %s: %s (quiet: watch.quietSessions)", c.what, quoted(held)))
		}
	}
	w.Host.logQuiet(quiet...)
	w.Sessions = cur
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
func (w *Watcher) quietSessions(names []string) (loud, quiet []string) {
	for _, n := range names {
		if slices.ContainsFunc(w.Host.cfg.Watch.QuietSessions, func(p string) bool { return alerts.Glob(p, n) }) {
			quiet = append(quiet, n)
		} else {
			loud = append(loud, n)
		}
	}
	return loud, quiet
}

// WatchParty is who the watch's events are by.
var WatchParty = state.Party{Name: "beekeeper watch"}

// Pending prints the notes and timers that fell due, the recorded sessions
// that ended, a relay taken or expired and the relay due at relayAt.
// The state keeps that they were reported, so each is one line however many
// watches run; the state is written only then.
func (w *Watcher) Pending(ctx context.Context, sessions []*claude.Session) {
	if ctx.Err() != nil {
		return
	}
	st, err := w.Host.readSnapshot()
	if err != nil {
		w.Fail("state-read", "cannot read the state: %v", err)
		return
	}
	w.Clear("state-read")
	if w.Chores {
		defer func() {
			if ctx.Err() == nil {
				w.claims(ctx)
			}
		}()
	}
	st = w.SyncExecutions(ctx, st)
	if ctx.Err() != nil {
		return
	}
	w.Records = st.Records
	supervised := w.CheckLoop(ctx, st)
	if ctx.Err() != nil {
		return
	}
	if w.Standby {
		if !w.guideRecovery {
			w.GuideGone(ctx, st, sessions)
			w.ResumeRestarted(ctx, guideRole, st, sessions)
		}
		for _, rl := range roles {
			w.RelayOverdue(ctx, rl, st, sessions)
		}
		w.importRows(ctx, st)
	}
	if !w.Standby || !supervised {
		w.GuideMonitor(ctx, st, sessions)
	}
	if w.Standby && supervised {
		return // the supervisor loop's watch reports them
	}
	if ctx.Err() != nil {
		return
	}
	w.SayStop(st)
	w.noteAnswers(st)
	w.Refusals()
	u := w.WatchUsage(ctx, st, sessions)
	if ctx.Err() != nil {
		return
	}
	if w.Chores && (u == nil || !u.High) {
		w.AutoResume(ctx, st) // the parks an earlier poll marked resumable
	}
	if w.Chores {
		w.StopParked(ctx, st, sessions) // a parked session costs no memory
	}
	if st.Stop == nil {
		w.StoppedAgents(st, sessions) // STOPPED names the sessions the stop ended
	}
	if ctx.Err() != nil {
		return
	}
	w.Capacity(ctx, st, sessions)
	w.ImportWaits(st)
	w.RowlessAgents(st)
	w.HandoversDue(st, sessions)
	w.doctor(ctx)
	if ctx.Err() != nil {
		return
	}
	signedIn := ProbeLogins(ctx, st.Notes)
	probed := ProbeHoldLifts(ctx, st.Holds, w.Host.now)
	if ctx.Err() != nil {
		return
	}
	var over, kept []overtake
	if len(OvertakeRefs(st)) > 0 {
		readRefs := func() error {
			over, kept = w.overtakenNow(ctx, st)
			return nil
		}
		if w.LoopOwned {
			if err := w.Host.loopGitHub(ctx, "watch", readRefs); err != nil {
				over, kept = nil, nil
			}
		} else {
			_ = readRefs()
		}
	}
	if ctx.Err() != nil {
		return
	}
	parks := w.SettledParks(ctx, st)
	if ctx.Err() != nil {
		return
	}
	if !w.Chores {
		w.wouldOvertake(st, over, kept)
		over, kept = nil, nil
	}
	githubHeld := loop.BoardBudgetLow(st.Budget, w.Host.cfg.GitHub.Floor, w.Host.now) || w.Host.resetFirst(st)
	var found map[int]checkResult
	if w.LoopOwned {
		found = w.Host.loopTimerChecks(ctx, st.Timers, w.Host.now, githubHeld)
	} else {
		found = checkTimers(ctx, st.Timers, w.Host.now, githubHeld)
	}
	if ctx.Err() != nil {
		return
	}
	if w.Chores {
		w.overrides(ctx, githubHeld)
	}
	if ctx.Err() != nil {
		return
	}
	var settles func(state.Timer) bool // nil: a running watch settles every timer
	if !w.Chores {
		w.wouldFireTimers(st, found)
		settles = func(state.Timer) bool { return false }
	}
	var fires []timerFire
	var defaulted, overtaken []state.Note
	fire := func(st *state.State) ([]string, []state.Event, bool) {
		lines, evs := CloseProbed(st, signedIn)
		hl, he := LiftProbed(st, probed)
		lines, evs = append(lines, hl...), append(evs, he...)
		nl, ne, nd := closeDefaulted(st, w.Host.cfg.Guide.Person, WatchParty, w.Host.now)
		lines, evs, defaulted = append(lines, nl...), append(evs, ne...), nd
		overtaken = heldAmong(st.Notes, over)
		ol, oe := closeOvertaken(st, over, WatchParty)
		lines, evs = append(lines, ol...), append(evs, oe...)
		ol, oe = keepOpen(st, kept, WatchParty)
		lines, evs = append(lines, ol...), append(evs, oe...)
		kl, ke := MarkResumable(st, w.Host.recheckLeaseParks(st, parks), w.Host.cfg.Agents.AutoResume, w.Host.now)
		lines, evs = append(lines, kl...), append(evs, ke...)
		tl, te, tf, touched := settleTimersMatching(st, found, w.Host.now, w.Host.cfg.Watch.Interval.Duration, settles)
		lines, evs, fires = append(lines, tl...), append(evs, te...), tf
		seen := touched
		pl, pe := FirePending(st, sessions, w.Host.now)
		lines, evs = append(lines, pl...), append(evs, pe...)
		ppl, ppe := ParkedNews(st, w.Host.cfg.Guide.Person, w.Host.now)
		lines, evs = append(lines, ppl...), append(evs, ppe...)
		return lines, evs, seen || len(lines) > 0 || len(evs) > 0
	}
	if _, _, changed := fire(st); !changed {
		return
	}
	var lines []string
	var due []dueItem
	var verdicts []state.WorkerReport
	err = w.Host.store.Update(func(st *state.State) ([]state.Event, error) {
		var evs []state.Event
		verdicts = slices.DeleteFunc(slices.Clone(st.WorkerReports), func(r state.WorkerReport) bool { return r.TellTo == (state.Party{}) })
		lines, evs, _ = fire(st)
		for _, l := range lines {
			evs = append(evs, event(WatchParty, "watch.pending", "%s", l))
		}
		w.Records = st.Records
		due = firedNow(st, w.Host.now)
		return evs, nil
	})
	if err != nil {
		w.Fail("state-write", "cannot write the state: %v", err)
		return
	}
	w.Clear("state-write")
	for _, l := range lines {
		w.OutputEvent(l)
	}
	w.TellReviewVerdicts(ctx, verdicts)
	for _, n := range defaulted {
		due = append(due, defaultedItem(&n))
	}
	for _, d := range due {
		w.Notify(ctx, notify.Due, d.key, d.summary, d.body)
	}
	for _, f := range fires {
		if f.T.Wake == "" && f.T.Run == "" {
			w.Notify(ctx, notify.Due, fmt.Sprintf("timer#%d", f.T.ID), fmt.Sprintf("beekeeper: timer #%d: %s", f.T.ID, truncate(f.Reason, 60)), truncate(f.T.What, 200))
		}
	}
	w.ActTimers(ctx, fires)
	if len(defaulted) > 0 {
		w.TimerActs.Go(func() {
			w.TellDefaulted(ctx, defaulted)
			for _, n := range defaulted {
				w.emitReleased(ctx, &n, ClosedDefaulted)
			}
		})
	}
	for _, n := range overtaken {
		w.TimerActs.Go(func() { w.emitReleased(ctx, &n, closedOvertaken) })
	}
}

// emitReleased releases the board items the closed decision n held and
// says so, one line each.
func (w *Watcher) emitReleased(ctx context.Context, n *state.Note, how string) {
	for _, l := range w.Host.release(ctx, WatchParty, n, how) {
		w.EmitNow("pending", "%s", l)
	}
}

// noteAnswers says the answers of the notes that closed since the last
// tick, once: NOTE ANSWERED. The log is read only when a note closed.
func (w *Watcher) noteAnswers(st *state.State) {
	open := make(map[int]bool, len(st.Notes))
	for _, n := range st.Notes {
		open[n.ID] = true
	}
	prev := w.openNotes
	w.openNotes = open
	if prev == nil {
		w.answersSince = w.Host.now
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
	evs, err := w.Host.store.Events(0, func(e state.Event) bool { return e.Verb == answerRecorded && e.At.After(since) })
	if err != nil {
		return // the next close reads them again
	}
	for _, e := range evs {
		w.answersSince = e.At
		if an, ok := parseAnswered(e); ok && an.ID != 0 && prev[an.ID] {
			w.EmitNow("pending", "%s", answeredLine(an.ID, cmp.Or(e.By.Person, e.By.Name), truncate(an.Answer, 300)))
		}
	}
}

// Refusals reports upstream refusals and recovered hook panics since the
// last reading. The first reading establishes a baseline and says none.
func (w *Watcher) Refusals() {
	evs, err := w.Host.store.Follow("watch.refused", func(e state.Event) bool { return e.Verb == verbHookRefused || e.Verb == "hook.panic" })
	if err != nil {
		return // the next reading reads them again
	}
	since := w.RefusedSince
	if since.IsZero() {
		w.RefusedSince = w.Host.now
		return
	}
	for _, e := range evs {
		if e.At.After(since) {
			w.RefusedSince = e.At
			if e.Verb == "hook.panic" {
				w.EmitNow("hook.panic", "HOOK PANIC %s", truncate(e.Detail, 400))
				continue
			}
			w.EmitNow("refused", "REFUSED %s: %s", cmp.Or(e.By.Name, e.By.Session), truncate(e.Detail, 400))
		}
	}
}

// StoppedAgents says once which agents with a task have no running CLI: a
// worker stopped by a reboot or a crash does not come back by itself, and
// its task waits until the supervisor resumes it.
func (w *Watcher) StoppedAgents(st *state.State, sessions []*claude.Session) {
	stopped := map[string]bool{}
	var now []string
	for _, ag := range stoppedAgents(st.Agents, sessions) {
		if st.Report.Running() && ag.Is(st.Report.Party) {
			continue // the reporter's turn ended: tendReporter ends it, nobody resumes it
		}
		if keptBy(st, ag, w.Host.now) != "" {
			continue // parked on purpose: a person or its timer brings it back
		}
		k := cmp.Or(ag.Session, ag.Name)
		last, gone := goneAgent(w.Host.cfg, st, sessions, ag, w.Host.cfg.Agents.StaleAfter.Duration, w.Host.now)
		if gone {
			k += "/gone"
		}
		stopped[k] = true
		if !w.stopped[k] {
			hint := resumeHint(ag)
			switch on := parkedOn(waitsOf(st.Records, ag), w.Host.cfg.Guide.Person); {
			case ag.Undelivered != "":
				hint = fmt.Sprintf("task not delivered: %s; beekeeper agents wake %q <message> delivers it", ag.Undelivered, ag.Name)
			case ag.Import.Pending(w.Host.now):
				hint = importStatus(ag, w.Host.now)
			case on != "":
				hint = fmt.Sprintf("parked on a person: %s; their answer resumes it", on)
			case resumedForTask(ag):
				hint = fmt.Sprintf("ended on a wait again after its resume at %s on %s: %s", clock(w.Host.now, ag.ResumedWait), ag.ResumedOn, hint)
			}
			if gone {
				hint = goneActivityPrefix + last.UTC().Format(time.RFC3339) + "; doctor retires it; " + hint
				if qerr := agyQuotaError(w.Host.cfg, ag); qerr != nil {
					hint = qerr.Error() + "; slot freed; doctor retires it and supervisor re-queues its task"
				}
			}
			now = append(now, fmt.Sprintf("%q (%s)", ag.Name, hint))
		}
	}
	w.stopped = stopped
	if len(now) > 0 {
		w.EmitNow("agents", "AGENTS STOPPED with a task and no running CLI: %s", strings.Join(now, ", "))
	}
}

// ImportWaits says once per wait which reopen waits to show its agent in
// the desktop, on what and until when: an import held by a person working
// in the desktop's window shows here instead of as an agent that never
// comes back.
func (w *Watcher) ImportWaits(st *state.State) {
	waits := map[string]time.Time{}
	var now []string
	for _, ag := range st.Agents {
		if !ag.Import.Pending(w.Host.now) {
			continue
		}
		k := cmp.Or(ag.Session, ag.Name)
		waits[k] = ag.Import.Since
		if !w.waits[k].Equal(ag.Import.Since) {
			now = append(now, fmt.Sprintf("%q (%s)", ag.Name, importStatus(ag, w.Host.now)))
		}
	}
	w.waits = waits
	if len(now) > 0 {
		w.EmitNow("agents-import", "IMPORT WAITS: %s", strings.Join(now, ", "))
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

// CheckLoop checks the supervisor, which is the supervisor loop, and
// reports whether it runs. While it does not the standby watch restarts
// its service (never during a stop) at most every loopRestartEvery, and
// once it stayed down past loopDownGrace every watch says SUPERVISOR LOOP
// DOWN and notifies (no-supervisor) until it runs again. No session is
// ever started or relayed for it.
func (w *Watcher) CheckLoop(ctx context.Context, st *state.State) bool {
	if w.Standby {
		w.Host.rearmRecovery()
	}
	if line, err := w.Host.checkDeliveries(w.Host.now); err != nil {
		w.Fail("delivery", "cannot read delivery health")
	} else {
		w.Clear("delivery")
		if line != "" {
			w.EmitNow("delivery", "%s", line)
		}
	}
	if line, err := w.Host.checkThroughput(w.Host.now, false); err != nil {
		w.Fail(watchThroughputKey, "release throughput unknown: cannot read durable health")
	} else {
		w.Clear(watchThroughputKey)
		if line != "" {
			w.EmitNow(watchThroughputKey, "%s", line)
		}
	}
	if w.LoopRunning() {
		if w.Stand.loopDown {
			w.EmitNow("supervisor", "SUPERVISOR LOOP BACK: the supervisor loop runs again (down since %s)", clock(w.Host.now, w.Stand.loopSince))
		}
		w.Stand.loopDown, w.Stand.loopSince = false, time.Time{}
		return true
	}
	if st.Stop != nil {
		// A stop holds the loop too: nothing restarts it, sayStop reports.
		w.Stand.loopSince = time.Time{}
		return false
	}
	if w.Stand.loopSince.IsZero() {
		w.Stand.loopSince = w.Host.now
	}
	restart := ""
	if w.Standby {
		restart = w.restartLoop(ctx)
	}
	if w.Host.now.Sub(w.Stand.loopSince) < loopDownGrace {
		return false
	}
	key := "loop@" + w.Stand.loopSince.UTC().Format(time.RFC3339)
	l := fmt.Sprintf("SUPERVISOR LOOP DOWN: the supervisor loop does not run since %s; no pull request is reviewed or merged on its go and no worker starts until it runs (%s)%s",
		clock(w.Host.now, w.Stand.loopSince), loopServiceHint(), restart)
	if !w.Stand.loopDown {
		w.Stand.loopDown = true
		w.EmitNow("supervisor", "%s", l)
	}
	w.Notify(ctx, notify.NoSupervisor, key, "beekeeper: the supervisor loop is down", l)
	return false
}

// UnitComingUp says a unit in this state runs or is on its way there.
func UnitComingUp(unitState string) bool {
	return unitState == unitActive || unitState == "activating" || unitState == "reloading"
}

// restartLoop starts the supervisor loop's service again (standby watch),
// at most every loopRestartEvery and only while the service is not
// starting already. It returns what the DOWN line adds.
func (w *Watcher) restartLoop(ctx context.Context) string {
	unit := w.Stand.LoopUnit
	if unit == nil {
		return "; this watch restarts no service here (only the standby, on Linux with systemd)"
	}
	if UnitComingUp(unit.State(ctx)) {
		return "; its service is starting"
	}
	if !w.Stand.loopRestart.IsZero() && w.Host.now.Sub(w.Stand.loopRestart) < loopRestartEvery {
		return fmt.Sprintf("; the standby watch restarted its service at %s", clock(w.Host.now, w.Stand.loopRestart))
	}
	w.Stand.loopRestart = w.Host.now
	if err := unit.Start(ctx); err != nil {
		w.EmitNow("supervisor-restart", "SUPERVISOR LOOP RESTART FAILED: %s: %v (beekeeper install installs it)", loopUnit, err)
		return fmt.Sprintf("; restarting its service failed: %v", err)
	}
	w.EmitNow("supervisor-restart", "SUPERVISOR LOOP RESTARTED: the standby watch started %s again", loopUnit)
	return "; the standby watch restarted its service"
}

// LoopService reads and starts the supervisor loop's user service.
type LoopService struct {
	State func(ctx context.Context) string
	Start func(ctx context.Context) error
}

// FirePending marks what is due or ended in st as reported and returns its
// watch lines and events; an event without a line is a recorded session
// that runs again.
func FirePending(st *state.State, sessions []*claude.Session, now time.Time) ([]string, []state.Event) {
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
		evs = append(evs, event(WatchParty, "note.due", "#%d %s", n.ID, n.Text))
	}
	for i := range st.Timers {
		t := &st.Timers[i]
		if t.Auto() || !state.Due(t.Due, t.Fired, now) {
			continue
		}
		t.Fired = now.UTC()
		lines = append(lines, fmt.Sprintf("TIMER: #%d due %s: %s (beekeeper timer done %d)", t.ID, clock(now, t.Due), truncate(t.What, 200), t.ID))
		evs = append(evs, event(WatchParty, "timer.due", "#%d %s", t.ID, t.What))
	}
	for i := range st.Records {
		r := &st.Records[i]
		_, live := claude.Live(sessions, r.Session)
		if live && !r.Ended.IsZero() {
			r.Ended = time.Time{} // resumed: its next end is reported again
			evs = append(evs, event(WatchParty, "session.resumed", "%s: %s", r.Session.Name, recordText(*r)))
		}
		if live || !r.Ended.IsZero() || rosterKept(st, r.Session, now) != "" {
			continue
		}
		r.Ended = now.UTC()
		lines = append(lines, fmt.Sprintf("SESSION ENDED: %q, which %s: re-query %s", r.Session.Name, truncate(recordText(*r), 200), r.Issue))
		evs = append(evs, event(WatchParty, "session.ended", "%s: %s", r.Session.Name, recordText(*r)))
	}
	for _, r := range st.WorkerReports {
		lines = append(lines, reportLines(r)...)
	}
	st.WorkerReports = nil
	return lines, evs
}

// TellReviewVerdicts wakes who started each review with its verdict: the
// verdict may be a plain comment on GitHub, which shows no review state. A
// wake that fails is said: the report above still carries the verdict.
func (w *Watcher) TellReviewVerdicts(ctx context.Context, reports []state.WorkerReport) {
	for _, r := range reports {
		to := cmp.Or(r.TellTo.Session, r.TellTo.HostSession, r.TellTo.Name)
		msg := fmt.Sprintf("The desk review %q reports:\n%s", r.By.Name, r.Text)
		if err := w.Host.wakeAgent(ctx, WatchParty, to, msg, ""); err != nil {
			w.EmitNow("pending", "REVIEW VERDICT NOT DELIVERED to %q from %q: %v", r.TellTo.Name, r.By.Name, err)
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

// WatchParkOnPerson reports whether p names a note or an issue or pull request
// waiting on a person's word. Operational targets and untargeted waits do
// not become person waits merely because their descriptions mention a person.
func WatchParkOnPerson(p *state.Park, person string) bool {
	if _, note := parkNote(p.On); note {
		return ParkOnPerson(p, person)
	}
	if _, _, note := parkCentralNote(p.On); note {
		return ParkOnPerson(p, person)
	}
	if _, ref := parseRef(p.On); !ref {
		return false
	}
	return ParkOnPerson(p, person)
}

// ParkOnPerson conservatively protects human waits from automatic wakes,
// including waits without a structured target.
func ParkOnPerson(p *state.Park, person string) bool {
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

// ParkedNews says once per park which agents parked on a person (a note,
// the person's or a colleague's answer, a review), which the supervisor
// carries to the guide, who tells the person, and which parked on a lease,
// with their place in its queue: the supervisor's grant settles that one.
func ParkedNews(st *state.State, person string, now time.Time) ([]string, []state.Event) {
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
			evs = append(evs, event(WatchParty, "agents.parked-on-lease", "%s: %s", ag.Name, res))
			continue
		}
		if !WatchParkOnPerson(p, person) {
			continue
		}
		p.Told = now.UTC()
		lines = append(lines, fmt.Sprintf("PARKED ON A PERSON: %q %s; tell the guide, who tells %s", ag.Name, parkedText(p, now), cmp.Or(person, "the person")))
		evs = append(evs, event(WatchParty, "agents.parked-on-person", "%s: %s", ag.Name, p.Waits))
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

// Runaways runs in the command, which holds what it needs (watchRunaways).
func (w *Watcher) Runaways(sessions []*claude.Session, t *proc.Table) {
	watchRunaways(w, sessions, t)
}

// SlotHolds says each build slot a run has held longer than
// memcap.holdBound: one SLOT HELD LONG line naming the slot, its holder and
// the command when it starts, one ENDED line when the slot is free again.
func (w *Watcher) SlotHolds() {
	bound := w.Host.cfg.Memcap.HoldBound.Duration
	if bound <= 0 {
		return
	}
	found := map[string]bool{}
	for _, s := range machine.ReadSlots(w.Host.slotView()) {
		h, ok := guard.ParseHolder(s.Holder)
		if s.Free || !ok || h.Since.IsZero() || w.Host.now.Sub(h.Since) < bound {
			continue
		}
		key := fmt.Sprintf("slothold:%d:%d:%s", s.N, h.PID, h.Since.UTC().Format(time.RFC3339))
		found[key] = true
		w.Emit(key, "SLOT HELD LONG: slot %d past memcap.holdBound (%s), held by %s — a benchmark or a lab proof runs with beekeeper run --class benchmark|lab, which takes no slot",
			s.N, dur(bound), guard.HolderText(s.Holder, w.Host.now))
	}
	w.ClearMissing("slothold:", found)
}

// StaleLeases runs in the command, which holds what it needs (watchStaleLeases).
func (w *Watcher) StaleLeases(ctx context.Context, sessions []*claude.Session) {
	watchStaleLeases(w, ctx, sessions)
}
