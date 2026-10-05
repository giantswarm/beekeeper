package cmd

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/notify"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/upgrade"
)

func (a *app) watchCmd() *cobra.Command {
	var once, notifyDesktop, standby bool
	c := &cobra.Command{
		Use:   "watch",
		Short: "Stay silent until something needs a look, then say it in one line",
		Long: `Poll the machine every watch.interval (30s) and print one line per event,
silent otherwise: made to be the source of a Monitor, so the supervisor
wakes only when something needs a look.

Threshold breaches (RAM, swap, desktop scope, load, CPU and memory
pressure, tmpfs, disk, the GitHub budget) and unreadable sources are one line when they
start and one ENDED line when they end, never repeated while they last.
OOM kills are never folded away: every poll reports every kill since the
last one, grouped by whose limit they hit; a cap kill whose scope no
run.start names says its cap is unknown. A kill in a test run's scope
(memcap-test-…, MEMCAP_TEST=1) is one quiet "test kill" line, never a
KERNEL OOM. Sessions that start, end or
restart are reported, and so is a lease whose holder is gone. A session
over a threshold in metrics.runaway (GitHub calls in the last hour, the
same failing tool call repeating in it, its context's fill) is one RUNAWAY
line per figure, once per watch. A lane whose first arrived merge has
waited longer than merge.stallAfter (5m) behind places whose merges are
not in the gate (beekeeper lanes) is one LANE STALLED line when it starts
and one ENDED line when it ends. A running merge whose gate process is
gone is one MERGE LOST line: it settles with an unknown release. A
running merge whose devctl runs on merge.hungAfter (45m) after its pull
request merged, or that long after its start once the pull request is
closed, is one MERGE HUNG line: the watch ends its devctl and records the
merge at the next poll. A
registered agent with a task and no running CLI is one AGENTS STOPPED
line with how to resume it, unless it is kept (agents keep, or a timer
that wakes it). Fewer busy agents than capacity.floor, with the headroom
for a start, is one CAPACITY LOW line, and capacity.ceiling reached one
CAPACITY FULL line, each with the headroom (beekeeper capacity) and one
ENDED line when the count recovers. A reopen that waits to show an agent in the desktop (the
window has the person's focus) is one IMPORT WAITS line per wait, with
what it waits for and until when. A settling merge leaves its lane
once the lane has settled (its release rolled and its HelmReleases Ready,
or no installation to roll), logged as lane.settled, silently, however
late; one not settled past merge.settleTimeout is one LANE STUCK line
with what the lane waits for and one ENDED line when it ends. A note or a
timer that falls due, the end of a session with a record (sessions serve)
and a supervisor relay taken or expired are one line each, once: the state keeps that they were reported, so
a second or restarted watch stays silent about them. An open note for
someone, not pinned and not a login, is overtaken once every issue and pull
request it names with note add --ref is closed or merged (one GraphQL read
a poll, none under the budget floor; a note with no --ref never is, guide
watch names it once its filing session is archived): one NOTE OVERTAKEN
line with the reason,
the note closed and note.overtaken logged; --once names each one it would
close and writes nothing. The
installations' alerts are read every alerts.every and each NEW or RESOLVED
one at or above its installation's floor is a line, a flapping one a single
FLAPPING line (beekeeper alerts watch); only one watch at a time reads them.
Every poll, a firing alert at alerts.pageSeverity in the baseline, of
alerts.team when it is set, that no session has owned (beekeeper alerts own) for alerts.ownerGrace is one line,
PAGE UNOWNED <installation> <alertname> <where> for <duration> (one per
alertname, the longest unowned, "(+n)" for the others), again every
alerts.ownerGrace while it stays unowned (page-unowned, critical, under
--notify); --once says each one due and writes nothing.

Every upgrades.every (5m) the same installations' Cluster API clusters are
read, and every watch.interval an installation an upgrade runs on or holds
(one list of Clusters, KubeadmControlPlanes, MachinePools and
MachineDeployments per installation, one per kind on one that does not serve
them all, within alerts.timeout, in parallel). The readings are shared
(upgrades.json): a second watch, snapshot and ui use a fresh one instead of
reading again. A
cluster whose release changes begins an upgrade: its release label differs
from the one cluster-api-events recorded, cluster-api-events marks it
upgrading, or its scheduled upgrade is due. It ends once none of that holds
and its control plane and node pools have rolled. While it runs, the
installation is held (hold upgrade:<installation>/<cluster>): the gate
refuses the merges of its lanes and lease claims of its name are refused.
"UPGRADE <installation>/<cluster> <from> → <to>" is said when it begins and
"UPGRADE ENDED …" when it ends, once each; an unreadable installation is one
line and keeps its holds.

Once the supervisor session's context (its transcript's last request, the
CTX column) reaches supervisor.relayAt, RELAY DUE is said at the first
quiet moment: no gated merge running or settling, no grant waiting to be
claimed, no claim queued and no relay open. It is said once per supervisor
and supervisor.relayAt, again after a relay is cancelled or expires, and
once more by every watch that did not say it yet (a restarted watch).

Every poll but --once's runs beekeeper doctor in the background (its last
run still going, the poll skips it): one DOCTOR line per agent it took off
the roster, desktop session it archived or retitled, fault it fixed or
note it filed, and one DOCTOR FAULT line while a known fault lasts.

A running beekeeper watch, this one included, whose binary self-update
replaced keeps the code it started with: one WATCH STALE line names the
watch, the version it runs and the one installed, and that a re-arm (a
restart) picks it up; one ENDED line follows once it exits. No watch
re-executes itself. --once says the stale watches it finds as well.

A registered agent whose session's context reaches agents.relayAt gets one
HANDOVER DUE "<agent>" at <n>k: beekeeper agents handover "<agent>" at its
first quiet moment: no tool command of its own running and no gated merge
of its own in flight.

--notify also sends the events that need a person to the desktop's
notification service (org.freedesktop.Notifications on the session bus):
the kinds in notify.kinds, a note or timer falling due (due), the GitHub
budget under the floor (budget), a stale lease (stale-lease), a
supervisor whose CLI stayed gone past supervisor.restartGrace with no relay
open (no-supervisor, critical) and an unowned page (page-unowned, critical). The machine's lines (memory, swap, OOMD
IMMINENT, OOM kills, load, processes) never notify: the supervisor acts on
them, and its watch says them. Each
event is one notification however many watches notify: the first to claim
it in notify.json sends it. A lasting condition and a supervisor gone
notify again after notify.repeat (30m); notify.quietHours hold
everything but a critical one and send what they held as one notification
when they end. Nothing routine notifies. With no notification service the
watch runs on, prints its lines and says so once.

--standby is for a watch that runs when no supervisor does (the
beekeeper-notify user unit): while a supervisor's session runs it leaves
the notes, timers, session records and relays to the supervisor's watch,
and it never reads the alerts, so it takes nothing from the supervisor's
view. It reads the upgrades and sets and lifts their holds only while no
supervisor's watch has begun an upgrade cycle within five watch.interval
(upgrades-watch.json), and says so once. It relays a supervisor or guide
that is still over its relayAt <role>.relayGrace (30m) after its relay
due, with no relay open and, for the supervisor, at a quiet moment: it
starts the successor as the holder's own relay does (RELAYED), and the
successor's start takes the role.

The quiet rules keep what is noise for the supervisor out of the output:
other teams' alerts matching alerts.quiet (by default, once alerts.team is
set, every other team's and team-less notify alert), an alert back after a reading that missed it with its old
start, and the start, end and restart of the short-lived sessions in
watch.quietSessions (by default beekeeper's tests, "test: *"). A rule
never holds back an alert of alerts.team, one on an installation in play
(leased, claimed or merged into within the last half hour, or with a merge
settling), or a page unless the rule names a cluster. What they hold back
is logged (beekeeper log --verb watch.quiet) and counted in the snapshot;
everything else is said as before.

The machine's numbers are sampled in a loop of their own, so a slow
installation read, a subprocess or a TLS timeout never delays a memory or
load line. The rest of a poll waits for a lane read or the budget probe one
watch.interval at most, and skips one whose previous run still goes instead
of starting a second.

The CPU is said before it saturates: HIGH LOAD once the 1-minute load
passes watch.loadMax (unset: watch.loadPerCoreMax, 1.5, × cores), LOAD
RISING once it passes one per core at more than twice the 5-minute load,
CPU PRESSURE once two samples in a row read some avg10 of
/proc/pressure/cpu over watch.cpuPSIMax (40 %); each names the five
commands that burned the most CPU since the last sample. While the load is
over its threshold or CPU pressure over watch.cpuPSIMax, the installation reads (upgrades,
alerts, lane settling) run every 4 × their interval at nice 10: one READS
SLOWED line when that starts and one ENDED line when it ends.

A fork storm is PROCESS STORM once two samples in a row read more forks a
second (/proc/stat) than watch.forkRateMax (50) over the machine's usual
rate (the last 10 minutes' average outside a storm), with the commands and
sessions of the processes started since the last sample; more than
watch.stackMax (3) copies of one command line from the same place in the
process tree, each running over a minute, are one STACKED line with the
count, the oldest's age, its parent and its session. More than
watch.toolProcsMax (1000) processes of the CLIs in watch.tools (kubectl,
helm, tsh, gh, flux, devctl) running machine-wide are one LOAD line with
their commands and the sessions that run them. Each ends with an ENDED
line; a negative threshold turns it off. A printed command line
keeps no value: the program, its subcommands and the flag names.

What a watch has said is kept per caller (seen.watch.<caller>.json): a
restarted watch of the same session says no open condition, runaway or
stale lease again, only its end or what is new. Runs until killed. --once
polls once, keeps no mark and says every condition it finds.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			w := a.newWatcher(standby, !once)
			if notifyDesktop {
				d := plat.NewNotifier()
				defer func() { _ = d.Close() }()
				w.notifier = notify.New(a.cfg.Notify.Policy(), a.cfg.StateDir, d, func(l string) { w.emitNow("notify", "%s", l) })
			}
			return w.run(ctx, once)
		},
	}
	c.Flags().BoolVar(&once, "once", false, "poll once and exit")
	c.Flags().BoolVar(&notifyDesktop, "notify", false, "send the events that need a person to the desktop too (notify.kinds)")
	c.Flags().BoolVar(&standby, "standby", false, "while a supervisor runs, leave its events to its watch and never read the alerts")
	return c
}

type watcher struct {
	*app
	mu   sync.Mutex
	last map[string]time.Time
	// active are the lasting conditions said and not yet ENDED.
	active map[string]condition
	// markFile keeps active and reported for a restarted watch; dirty is
	// set when they changed since it was written.
	markFile   string
	dirty      bool
	lastPoll   time.Time
	lastBudget time.Time
	scopeOOM   int64
	sessions   map[string]*claude.Session
	seenKills  map[string]bool
	// reported are the stale leases said once.
	reported map[string]bool
	// notifier sends the events that need a person (--notify); nil prints only.
	notifier *notify.Notifier
	// swapSamples are the last hour's swap readings, oldest first: the
	// growth rate toward systemd-oomd's trigger.
	swapSamples []swapSample
	// swap is the machine sample's latest swap reading, under mu: the
	// poll's CAPACITY lines weigh a start against it.
	swap *swapReading
	// readHeadroom reads the headroom of a start; nil is app.readHeadroom.
	readHeadroom func(context.Context, *swapReading) *headroom
	// standby leaves a running supervisor's events to its watch.
	standby bool
	// upgrades are the last upgrade readings of the installations.
	upgrades upgrade.Readings
	// gap is the term of the gone supervisor this watch said, until a
	// supervisor is back.
	gap string
	// records are the session records of the last poll: their sessions'
	// ends get the record's line instead of the SESSIONS ended one.
	records []state.Record
	// stopped are the agents with a task this watch said have no running
	// CLI, by session key, until their CLI runs again.
	stopped map[string]bool
	// waits are the reopen waits this watch said, by agent session and the
	// wait's start, until they end.
	waits map[string]time.Time
	// dues are the supervisor's relay dues this watch said; nil in a watch
	// --once.
	dues relayDues
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
	// readForks reads the fork counter; nil is plat.Machine.Forks.
	readForks func() (uint64, error)
	// owners names the session of each CLI PID the last poll found: the
	// machine sample attributes a storm or a stack with it.
	owners atomic.Pointer[map[int]string]
	// strained is the machine sample's verdict that the CPU is saturated:
	// the installation reads slow down (READS SLOWED).
	strained atomic.Bool
	// settling and budgeting are set while a lane read or a budget probe
	// runs: the next poll skips it instead of queueing a second one.
	settling, budgeting atomic.Bool
	lastSettle          time.Time
	// sweeping is set while the exposure sweep runs, lastSweep when it began.
	sweeping  atomic.Bool
	lastSweep time.Time
	// teleporting is set while the Teleport login is read, lastTeleport
	// when it began.
	teleporting  atomic.Bool
	lastTeleport time.Time
	// centraling is set while the roster is published to the central
	// instance, lastCentral when that last started and centralChecked when
	// the central instance was last called.
	centraling     atomic.Bool
	lastCentral    time.Time
	centralChecked time.Time
	// polls counts the polls begun.
	polls atomic.Int64
	// missing are the sections whose platform part this build does not
	// have, said once each.
	missing map[string]bool
	// chores runs the doctor every poll (not for --once); doctoring is set
	// while its run goes, and retitled holds when it last retitled each
	// desktop session.
	chores    bool
	doctoring atomic.Bool
	retitled  map[string]time.Time
	// timerActs are the fired timers' wakes and commands under way, and
	// the defaulted decisions' filers being told.
	timerActs sync.WaitGroup
	// openNotes are the notes open at the last tick, answersSince the time
	// up to which their answers were said.
	openNotes    map[int]bool
	answersSince time.Time
	// replaced says whether a process's binary was replaced, and its path;
	// nil is platform.ProcessBinary. versionOf is the version a binary file
	// reports; nil runs it.
	replaced  func(pid int) (string, bool)
	versionOf func(ctx context.Context, file string) string
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
	return readHelmReleases(ctx, lane)
}

// watchClock is a watch's time: its loops wait on its timers.
type watchClock interface {
	Now() time.Time
	// Timer fires once after d; stop releases it.
	Timer(d time.Duration) (fire <-chan time.Time, stop func())
}

// wallClock is the time a watch keeps outside its tests.
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

func (w *watcher) clk() watchClock {
	if w.clock != nil {
		return w.clock
	}
	return wallClock{}
}

func (w *watcher) run(ctx context.Context, once bool) error {
	w.lastPoll = w.clk().Now().Add(-w.cfg.Watch.Interval.Duration)
	s, err := plat.Machine.DesktopScope()
	if s != nil {
		w.scopeOOM = s.OOMKills
	}
	if !w.unavailable(secScope, err) {
		w.check("noscope", s == nil, "no Claude Desktop scope found; watching the machine numbers only")
	}
	interval := w.cfg.Watch.Interval.Duration
	if once {
		w.upgradeCycle(ctx)
		w.sample(ctx)
		w.lastSweep = time.Now() // swept here, not in the poll's background
		w.exposures()
		w.poll(ctx)
		w.stand.inflight.Wait() // a successor's start outlives no watch
		w.timerActs.Wait()      // nor a timer's wake or command
		return nil
	}
	// The machine is sampled in a loop of its own, so no network read or
	// subprocess of the rest of the poll ever delays a memory or load line.
	// The first sample comes before the reads start: they begin knowing
	// whether the machine is strained. The loop's own first sample is one
	// interval later: two samples in a row span one (CPU PRESSURE).
	w.sample(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
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
	}
	wg.Go(func() { w.loop(ctx, interval, true, w.upgradeCycle) })
	w.loop(ctx, interval, false, w.poll)
	return nil
}

// slowReads is how many times less often the installation reads run while
// the machine is strained.
const slowReads = 4

// loop runs fn every interval until ctx ends. A run that overruns skips the
// ticks it missed instead of queueing them. An installation read (reads)
// runs every slowReads × interval, at nice 10, while the machine is
// strained.
func (w *watcher) loop(ctx context.Context, interval time.Duration, reads bool, fn func(context.Context)) {
	clk := w.clk()
	for {
		start, every, rctx := clk.Now(), interval, ctx
		if reads {
			every, rctx = w.readEvery(interval), w.readCtx(ctx)
		}
		fn(rctx)
		next := start.Add(every)
		now := clk.Now()
		for !next.After(now) {
			next = next.Add(every)
		}
		fire, stop := clk.Timer(next.Sub(now))
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
func inFlight(ctx context.Context, wait time.Duration, running *atomic.Bool, fn func(context.Context)) {
	if !running.CompareAndSwap(false, true) {
		return
	}
	done := make(chan struct{})
	go func() {
		defer running.Store(false)
		defer close(done)
		fn(ctx)
	}()
	t := time.NewTimer(wait)
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
	store := alerts.NewStore(w.cfg.StateDir)
	defer func() { _ = store.Release() }()
	other := 0
	w.loop(ctx, w.cfg.Alerts.Every.Duration, true, func(ctx context.Context) {
		owned, owner, err := store.Own()
		switch {
		case err != nil:
			w.emit("alerts", "ALERTS baseline unusable: %v", err)
		case !owned:
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
			for _, l := range w.alertCycle(ctx, store) {
				w.emitNow("alerts", "%s", l)
			}
		}
	})
}

// emit says a lasting condition once, when it starts. While it lasts it is
// silent and returns its line at most every watch.repeat, for the
// notification a person gets (notify.repeat decides whether it goes out).
func (w *watcher) emit(key, format string, args ...any) string {
	line := fmt.Sprintf(format, args...)
	w.mu.Lock()
	now := time.Now()
	if w.active == nil {
		w.active = map[string]condition{}
	}
	_, active := w.active[key]
	if active && now.Sub(w.last[key]) < w.cfg.Watch.Repeat.Duration {
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
	Conditions map[string]condition `json:"conditions"`
	Reported   map[string]bool      `json:"reported"`
}

// newWatcher is a watch; one that keeps a mark resumes its caller's last
// watch (seen.watch.<caller>.json): it says no open condition or event
// again. --once and a watch outside a Claude session keep none.
func (a *app) newWatcher(standby, keep bool) *watcher {
	w := &watcher{app: a, standby: standby, last: map[string]time.Time{}, seenKills: map[string]bool{},
		reported: map[string]bool{}, active: map[string]condition{}, chores: keep, retitled: map[string]time.Time{},
		upgrades: upgrade.Readings{}}
	if keep {
		w.dues = relayDues{}
	}
	w.stand = standbyWatch{send: a.peerSend, open: plat.Opener.Open, succeed: a.succeedFromWatch, revive: a.reviveFromWatch, turning: unitsTurning, reopening: unitsReopening}
	if me, err := a.caller(); keep && err == nil {
		w.markFile = "seen.watch." + fileKey(me) + ".json"
		var m watchMark
		if found, err := a.store.ReadFile(w.markFile, &m); err == nil && found {
			maps.Copy(w.active, m.Conditions)
			maps.Copy(w.reported, m.Reported)
		}
	}
	return w
}

// saveMark keeps what the watch has said for its successor, when it changed.
func (w *watcher) saveMark() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.markFile == "" || !w.dirty {
		return
	}
	w.dirty = false
	_ = w.store.WriteFile(w.markFile, watchMark{Conditions: w.active, Reported: w.reported})
}

// notify sends one event that needs a person (--notify): a lasting kind
// takes no key.
func (w *watcher) notify(ctx context.Context, kind, key, summary, body string) {
	w.notifyAt(ctx, w.now, kind, key, summary, body)
}

// notifyAt is notify for a loop of its own, which keeps its own time.
func (w *watcher) notifyAt(ctx context.Context, now time.Time, kind, key, summary, body string) {
	if w.notifier != nil && body != "" {
		w.notifier.Notify(ctx, now, kind, key, summary, body)
	}
}

// swapSample is one poll's swap and MemAvailable.
type swapSample struct {
	at                         time.Time
	usedMiB, diskMiB, availMiB int
}

// swapWindow is how far back the swap growth rate looks, and minSwapSpan
// the shortest span it is measured over.
const (
	swapWindow  = time.Hour
	minSwapSpan = 5 * time.Minute
)

// swapTrend is the swap's movement over the last hour: disk swap's growth,
// the growth of all swap in use (what systemd-oomd measures), and whether
// MemAvailable fell. Rated once the readings span minSwapSpan.
type swapTrend struct {
	DiskPerHourMiB int
	UsedPerHourMiB int
	AvailFalling   bool
	Rated          bool
}

// pressing reports disk swap growing while MemAvailable falls: the only
// swap reading that means memory pressure. Swapped pages stay in swap long
// after the burst that pushed them out, and zswap's share sits in RAM.
func (t swapTrend) pressing() bool { return t.Rated && t.DiskPerHourMiB > 0 && t.AvailFalling }

// swapTrend records a reading and returns the movement over the last hour.
func (w *watcher) swapTrend(now time.Time, m machine.Mem) swapTrend {
	w.swapSamples = append(w.swapSamples, swapSample{now, m.SwapUsedMiB, m.DiskSwapMiB(), m.AvailableMiB})
	i := 0
	for i < len(w.swapSamples)-1 && now.Sub(w.swapSamples[i].at) > swapWindow {
		i++
	}
	w.swapSamples = w.swapSamples[i:]
	first := w.swapSamples[0]
	span := now.Sub(first.at)
	if span < minSwapSpan {
		return swapTrend{}
	}
	perHour := func(from, to int) int { return int(float64(to-from) / span.Hours()) }
	return swapTrend{
		DiskPerHourMiB: perHour(first.diskMiB, m.DiskSwapMiB()),
		UsedPerHourMiB: perHour(first.usedMiB, m.SwapUsedMiB),
		AvailFalling:   m.AvailableMiB < first.availMiB,
		Rated:          true,
	}
}

// swapLine says swap in use as disk plus zswap, systemd-oomd's swap rule
// (its trigger only when it watches a cgroup for swap) and disk swap's
// growth: the numbers that decide whether swap means pressure.
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
	line += fmt.Sprintf(", disk %+d MiB/h over the last hour", t.DiskPerHourMiB)
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
	th := w.cfg.Watch
	headroom := oomd.HeadroomMiB(m)
	if headroom < th.OOMDHeadroomMin(m.SwapTotalMiB) {
		return true
	}
	return t.UsedPerHourMiB > 0 && untilTrigger(headroom, t.UsedPerHourMiB) < th.OOMDWithin.Duration
}

// modelServer says the host models no model-server lease covers and
// unloads those a lab loaded or a budget exceeds, unless ollama.nameOnly.
func (w *watcher) modelServer(ctx context.Context, models []machine.HostModel) {
	holders, err := lease.Dir(w.cfg.LeaseDir).List()
	if err != nil {
		return
	}
	bs := modelBreaches(models, holders, w.cfg.Resources, w.cfg.Ollama.BudgetGiB)
	unloaded := map[string]error{}
	for _, b := range bs {
		if b.Unload && !w.cfg.Ollama.NameOnly {
			unloaded[b.Model.Name] = b.Model.Unload(ctx, serverURL(w.cfg, b.Model))
		}
	}
	w.check("modelserver", len(bs) > 0, "MODEL SERVER: %s", modelServerLine(bs, unloaded))
}

// emitNow prints an event that is never folded away.
func (w *watcher) emitNow(_ string, format string, args ...any) {
	w.emitLine(time.Now().Format("15:04:05") + " " + fmt.Sprintf(format, args...))
}

// emitLine prints one complete line.
func (w *watcher) emitLine(line string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = fmt.Fprintln(w.out, line)
}

// sample reads the machine's numbers (memory, swap, load, pressure, disk,
// the desktop scope) from /proc and /sys, and decides whether the machine
// is strained. It keeps its own time: it runs in a loop of its own.
func (w *watcher) sample(ctx context.Context) {
	now := time.Now()
	th := w.cfg.Watch

	// GTT is RAM the iGPU pins outside every cgroup: above its threshold
	// it is named as the cause of low memory, with the model servers' models.
	models, merr := hostModels(ctx, w.cfg)
	if merr == nil {
		w.modelServer(ctx, models)
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
		swapoff := plat.Machine.SwapoffRuns()
		w.check("swapoff", swapoff, "SWAPOFF IN PROGRESS: %s", line)
		if m.SwapTotalMiB > 0 {
			w.check("oomd", !swapoff && w.oomdImminent(m, oomd, t), "OOMD IMMINENT: %s", line)
		}
		w.keepSwap(&swapReading{At: now, UsedMiB: m.SwapUsedMiB, DiskMiB: m.DiskSwapMiB(), ZswapMiB: m.ZswappedMiB,
			PerHourMiB: t.DiskPerHourMiB, AvailFalling: t.AvailFalling, Rated: t.Rated})
	}
	w.sampleCPU(now)
	psi, err := plat.Machine.MemoryPressure()
	w.unavailable(secPressure, err)
	if err == nil {
		w.check("psi", psi > th.PSIMax, "MEMORY PRESSURE: full avg60 %.0f%%", psi)
	}
	if d, err := machine.ReadDisk("/tmp"); err == nil {
		w.check("tmp", d.UsedMiB > th.TmpMax(d.TotalMiB), "TMPFS /tmp: %d MiB", d.UsedMiB)
	}
	if d, err := machine.ReadDisk("/"); err == nil {
		w.check("disk", d.FreeMiB < th.DiskMin(d.TotalMiB), "LOW DISK: / %d GiB free", d.FreeMiB/1024)
	}
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
}

// sampleCPU says the CPU lines: HIGH LOAD over watch.loadMax (or
// watch.loadPerCoreMax × cores), LOAD RISING on a steep climb, CPU PRESSURE
// once two samples in a row read some avg10 over watch.cpuPSIMax, each
// with the top CPU consumers since the last sample; and READS SLOWED while
// the machine is strained.
func (w *watcher) sampleCPU(now time.Time) {
	th := w.cfg.Watch
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
	if t, err := plat.Machine.Processes(); err == nil {
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

// sampleProcs says PROCESS STORM once two samples in a row read a fork rate
// more than watch.forkRateMax over the machine's usual one, LOAD while more
// than watch.toolProcsMax processes of watch.tools run, and a STACKED line
// for each command line running more than watch.stackMax times over, each
// with an ENDED line when it ends.
// prev is the process table the last sample read, span ago.
func (w *watcher) sampleProcs(now time.Time, span time.Duration, prev, t *proc.Table) {
	th := w.cfg.Watch
	var owners map[int]string
	if o := w.owners.Load(); o != nil {
		owners = *o
	}
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
	w.table = t
	sessions := claude.Discover(w.cfg, t, w.now)
	owners := make(map[int]string, len(sessions))
	for _, s := range sessions {
		owners[s.PID] = s.Name
	}
	w.owners.Store(&owners)
	w.kills(ctx, since, sessions, t)
	w.tendReporter(ctx, sessions)
	w.tendFeedback(ctx)
	w.pending(ctx, sessions)
	w.sessionChanges(sessions)
	w.staleLeases(ctx, sessions)
	w.unownedPages(ctx, sessions)
	w.runaways(sessions, t)
	w.staleWatches(ctx, t)
}

// poll does everything but the machine sample: the process table, the
// sessions, the lanes and the budget. It waits for a lane read or a budget
// probe for one watch.interval at most.
func (w *watcher) poll(ctx context.Context) {
	w.polls.Add(1)
	w.now = w.clk().Now()
	since := w.lastPoll
	w.lastPoll = w.now
	th := w.cfg.Watch

	t, err := plat.Machine.Processes()
	switch {
	case w.unavailable(secSessions, err):
		// No session is known: what reads them is left out, not guessed.
	case err != nil:
		w.emit("proc", "cannot read the process table: %v", err)
		return
	default:
		w.clear("proc")
		w.pollSessions(ctx, since, t)
	}
	w.lostMerges(ctx)
	w.closeToolWindow(ctx, watchParty)
	w.stalls()
	w.vaultWaits()
	now := w.now
	if now.Sub(w.lastSettle) >= w.readEvery(th.Interval.Duration) {
		w.lastSettle = now
		inFlight(w.readCtx(ctx), th.Interval.Duration, &w.settling, func(ctx context.Context) {
			_ = w.checkPlaces(ctx, watchParty, now, "", "", 0, th.Interval.Duration)
			w.hungMerges(ctx, now)
			w.settled(ctx, now)
		})
	}
	if now.Sub(w.lastBudget) >= th.BudgetEvery.Duration {
		w.lastBudget = now
		inFlight(ctx, th.Interval.Duration, &w.budgeting, func(ctx context.Context) { w.budget(ctx, now) })
	}
	if now.Sub(w.lastTeleport) >= w.readEvery(th.Interval.Duration) {
		w.lastTeleport = now
		inFlight(ctx, th.Interval.Duration, &w.teleporting, w.teleport)
	}
	if w.cfg.Central.Enabled() && now.Sub(w.lastCentral) >= w.readEvery(th.Interval.Duration) {
		w.lastCentral = now
		inFlight(ctx, th.Interval.Duration, &w.centraling, w.syncRoster)
	}
	if now.Sub(w.lastSweep) >= w.readEvery(th.Interval.Duration) {
		w.lastSweep = now
		// The sweep takes a while on a big home directory: the poll does not wait.
		inFlight(ctx, 0, &w.sweeping, func(context.Context) { w.exposures() })
	}
	w.saveMark()
	if w.notifier != nil {
		w.notifier.Flush(ctx, w.now)
	}
}

// teleport says the Teleport login's trouble, one line when it starts and
// one ENDED line when it ends: about to expire, expired, or its keeper's
// renewal failed.
func (w *watcher) teleport(ctx context.Context) {
	v := w.readTeleport(ctx)
	if v == nil {
		return
	}
	for _, k := range teleportKeys {
		if k != v.Key {
			w.clear(k)
		}
	}
	if v.Key != "" {
		w.emit(v.Key, "%s", v.line(time.Now()))
	}
}

// vaultKey starts the condition key of a call waiting on the vault.
const vaultKey = "vault "

// vaultWaits says each call that waits on the person's unlock of the vault
// (secret.session), one VAULT LOCKED line naming who waits on what, and one
// ENDED line once it goes on or gives up. Nothing asks the person.
func (w *watcher) vaultWaits() {
	if !w.cfg.Secret.Session {
		return
	}
	path, err := secret.WaitsPath()
	if err != nil {
		return
	}
	ws, err := secret.ReadWaits(path)
	if err != nil {
		w.emit(vaultKey+"read", "cannot read the vault's waiting calls: %v", err)
		return
	}
	found := map[string]bool{}
	for _, v := range ws {
		key := vaultKey + v.Who + " " + v.Ref
		found[key] = true
		w.emit(key, "VAULT LOCKED: %s waits on %s since %s; the person unlocks it with beekeeper secret unlock in their own terminal",
			v.Who, v.Ref, v.Since.Local().Format("15:04"))
	}
	w.clearMissing(vaultKey, found)
}

// exposedKey starts the condition key of a credential exposed on disk.
const exposedKey = "exposed "

// exposures says each credential left exposed on disk (guard.Sweep), one
// EXPOSED line naming the file when it is found, never its content, and one
// ENDED line once it is fixed.
func (w *watcher) exposures() {
	o := w.cfg.Outbound
	found := map[string]bool{}
	for _, e := range guard.Sweep(o.SweepRoots, o.SweepDepth) {
		key := exposedKey + e.Path
		found[key] = true
		w.emit(key, "EXPOSED %s: %s", e.Path, e.What)
	}
	w.clearMissing(exposedKey, found)
}

// budget probes the GitHub budget and says when it is under the floor.
func (w *watcher) budget(ctx context.Context, now time.Time) {
	b, err := w.probeBudget(ctx)
	switch {
	case err != nil && ctx.Err() != nil:
	case err != nil:
		w.emit("budget-error", "GitHub budget unknown: %v", err)
	default:
		w.clear("budget-error")
		l := w.check("budget", b.Remaining < w.cfg.GitHub.Floor, "GITHUB BUDGET %d of %d: hold GitHub work until %s",
			b.Remaining, b.Limit, b.Reset.Local().Format("15:04"))
		w.notifyAt(ctx, now, notify.Budget, "", "beekeeper: GitHub budget under the floor", l+"\nbeekeeper budget")
	}
}

// stalls says each stalled lane, one LANE STALLED line per lane and waiting
// merge when the stall starts and one ENDED line when it ends.
func (w *watcher) stalls() {
	st, err := w.store.Read()
	if err != nil {
		return
	}
	stalled := map[string]bool{}
	for _, v := range w.laneViews(st) {
		if v.Stall != nil {
			key := "stall:" + v.Name + ":" + v.Stall.Merge.Key()
			stalled[key] = true
			w.emit(key, "LANE STALLED %s: %s", v.Name, w.stallText(*v.Stall))
		}
	}
	w.clearMissing("stall:", stalled)
}

// lostMerges records the outcome of each running merge whose gate process
// and devctl are gone (recordGone), one MERGE RECORDED or MERGE LOST line
// each. The gate itself prunes a lost merge only when the lane's next merge
// arrives, and until then the lane shows a merge running that no process
// runs.
func (w *watcher) lostMerges(ctx context.Context) {
	recorded, lost := w.recordGone(ctx)
	for _, d := range recorded {
		w.emitNow("lanes", "MERGE RECORDED: %s", d)
	}
	for _, m := range lost {
		w.emitNow("lanes", "MERGE LOST: %s in lane %s by %q: its gate (pid %d) and devctl are gone, whether it merged is unknown; the lane settles until %s, then frees once its HelmReleases are Ready",
			m.Key(), m.Lane, m.By.Name, m.PID, clock(w.now, w.now.Add(w.cfg.Merge.Settle.Duration)))
	}
}

// hungMerges ends the devctl of each running merge that has outlived its
// pull request (endHung), one MERGE HUNG line each; the next poll records
// it (lostMerges), or its gate does.
func (w *watcher) hungMerges(ctx context.Context, now time.Time) {
	for _, h := range w.endHung(ctx, watchParty, now, w.cfg.Merge.HungAfter.Duration, func(m state.Merge) bool {
		return now.Sub(m.Started) > w.cfg.Merge.HungAfter.Duration
	}) {
		w.emitNow("lanes", "MERGE HUNG: %s in lane %s by %q: %s, its devctl (pid %d) still ran: ended, the lane records it next",
			h.merge.Key(), h.merge.Lane, h.merge.By.Name, w.pullText(h.pull), h.merge.Child)
	}
}

// settled drops the settling merges whose lane has settled, so lanes shows
// the lane free before its next merge starts: a lane with no installation
// has nothing to roll, and one whose release rolled and whose HelmReleases
// are Ready is done, however late. A merge not settled past
// merge.settleTimeout is one LANE STUCK line with what the lane waits for,
// and one ENDED line once it settles or the lane is cleared.
func (w *watcher) settled(ctx context.Context, now time.Time) {
	st, err := w.store.Read()
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
		lane, ok := w.cfg.LaneNamed(m.Lane)
		if !ok || lane.Installation == "" {
			done[m.Key()] = "no installation to roll"
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
		} else if ready, wait := merge.Ready(lane, r.hrs, &m, now, w.cfg.Merge.Settle.Duration); ready {
			done[m.Key()] = "rolled, HelmReleases of " + lane.Installation + " Ready"
			if wait != "" {
				done[m.Key()] = "settled without a roll, " + wait
			}
			continue
		} else {
			why = wait
		}
		if since := now.Sub(m.Finished); since > w.cfg.Merge.SettleTimeout.Duration {
			key := "stuck:" + lane.Name + ":" + m.Key()
			stuck[key] = true
			w.emit(key, "LANE STUCK %s: %s has not settled %s after its merge: %s; fix the installation or clear the lane (beekeeper lanes clear %s)",
				lane.Name, m.Key(), dur(since), why, lane.Name)
		}
	}
	w.clearMissing("stuck:", stuck)
	if len(done) == 0 {
		return
	}
	var settled []state.Merge
	_ = w.store.Update(func(st *state.State) ([]state.Event, error) {
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
		w.leaveCentral(ctx, []state.Merge{m}, done[m.Key()])
	}
}

// commandTimeout bounds one journalctl or docker call of a poll: a wedged
// daemon must not stall the watch or its shutdown.
const commandTimeout = 20 * time.Second

func (w *watcher) kills(ctx context.Context, since time.Time, sessions []*claude.Session, t *proc.Table) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	kills, err := plat.Machine.OOMKills(ctx, since.Add(-2*time.Second))
	if err != nil {
		if ctx.Err() != nil || w.unavailable(secOOM, err) {
			return
		}
		w.emit("journal", "cannot read the kernel journal: %v", err)
		return
	}
	w.clear("journal")
	var fresh []oomKill
	var clusters []machine.Cluster
	runs := &runIndex{store: w.store}
	for _, k := range kills {
		key := fmt.Sprintf("%d@%d", k.PID, k.At.Unix())
		if w.seenKills[key] {
			continue
		}
		w.seenKills[key] = true
		if clusters == nil && strings.Contains(k.Memcg, "docker-") {
			clusters, _ = machine.KindClusters(ctx)
		}
		fresh = append(fresh, oomKill{OOMKill: k, Owner: oomOwner(k, clusters, sessions, t, runs)})
	}
	real, tests := splitTestKills(fresh)
	for _, line := range groupKills(real) {
		w.emitNow("kern", "KERNEL OOM: %s", line)
	}
	for _, line := range groupKills(tests) {
		w.emitNow("testkill", "test kill: %s", line)
	}
	if lines, err := plat.Machine.OomdKills(ctx, since.Add(-2*time.Second)); err == nil {
		for _, l := range lines {
			if !w.seenKills[l] {
				w.seenKills[l] = true
				w.emitNow("oomd", "SYSTEMD-OOMD: %s", truncate(l, 200))
			}
		}
	}
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
	w.logQuiet(quiet...)
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
		if slices.ContainsFunc(w.cfg.Watch.QuietSessions, func(p string) bool { return alerts.Glob(p, n) }) {
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
	st, err := w.store.Read()
	if err != nil {
		w.emit("state-read", "cannot read the state: %v", err)
		return
	}
	w.clear("state-read")
	w.records = st.Records
	supervised := w.supervisorGone(ctx, st, sessions)
	if w.standby {
		w.guideGone(ctx, st, sessions)
		w.resumeRestarted(ctx, guideRole, st, sessions)
		for _, rl := range roles {
			w.relayOverdue(ctx, rl, st, sessions)
		}
	}
	if w.standby && supervised {
		w.resumeRestarted(ctx, supervisorRole, st, sessions)
		return // the supervisor's watch reports them
	}
	w.noteAnswers(st)
	if w.chores {
		w.autoResume(ctx, st) // the parks an earlier poll marked resumable
	}
	w.stoppedAgents(st, sessions)
	w.capacity(ctx, st, sessions)
	w.importWaits(st)
	q := w.quietness(ctx, st, sessions)
	w.handoversDue(st, sessions)
	w.doctor(ctx)
	signedIn := probeLogins(ctx, st.Notes)
	probed := probeHoldLifts(ctx, st.Holds, w.now)
	over := w.overtakenNow(ctx, st)
	parks := w.settledParks(ctx, st)
	if !w.chores {
		w.wouldOvertake(st, over)
		over = nil
	}
	held := checkTimers(ctx, st.Timers, w.now, lowBudget(st.Budget, w.cfg.GitHub.Floor, w.now))
	var fires []timerFire
	var defaulted []state.Note
	fire := func(st *state.State) ([]string, []state.Event, bool) {
		seen, ce := observeCLI(st, sessions, w.now)
		lines, evs := closeProbed(st, signedIn)
		hl, he := liftProbed(st, probed)
		lines, evs = append(lines, hl...), append(evs, he...)
		nl, ne, nd := closeDefaulted(st, w.cfg.Guide.Person, watchParty, w.now)
		lines, evs, defaulted = append(lines, nl...), append(evs, ne...), nd
		ol, oe := closeOvertaken(st, over, watchParty)
		lines, evs = append(lines, ol...), append(evs, oe...)
		kl, ke := markResumable(st, parks, w.cfg.Agents.AutoResume, w.now)
		lines, evs = append(lines, kl...), append(evs, ke...)
		tl, te, tf, touched := settleTimers(st, held, w.now)
		lines, evs, fires = append(lines, tl...), append(evs, te...), tf
		seen = seen || touched
		pl, pe := firePending(st, sessions, w.now)
		lines, evs = append(lines, pl...), append(evs, pe...)
		for _, e := range ce {
			lines = append(lines, fmt.Sprintf("SUPERVISOR RESTARTED: %q, %s; it keeps the role", e.By.Name, e.Detail))
		}
		evs = append(evs, ce...)
		rl, re := fireRelay(st, w.now)
		dl, de := fireRelayDue(st, q, w.dues, w.now)
		lines, evs = append(append(lines, rl...), dl...), append(append(evs, re...), de...)
		return lines, evs, seen || len(lines) > 0 || len(evs) > 0
	}
	if _, _, changed := fire(st); !changed {
		return
	}
	var lines []string
	var due []dueItem
	err = w.store.Update(func(st *state.State) ([]state.Event, error) {
		var evs []state.Event
		lines, evs, _ = fire(st)
		w.records = st.Records
		due = firedNow(st, w.now)
		return evs, nil
	})
	if err != nil {
		w.emit("state-write", "cannot write the state: %v", err)
		return
	}
	w.clear("state-write")
	for _, l := range lines {
		w.emitNow("pending", "%s", l)
	}
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
	for _, n := range defaulted {
		w.timerActs.Go(func() { w.tellFiler(ctx, watchParty, &n, defaultedLine(&n)) })
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
		w.answersSince = w.now
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
	evs, err := w.store.Events(0, func(e state.Event) bool { return e.Verb == noteAnswered && e.At.After(since) })
	if err != nil {
		return // the next close reads them again
	}
	for _, e := range evs {
		w.answersSince = e.At
		if an, ok := parseAnswered(e); ok && prev[an.ID] {
			w.emitNow("pending", "%s", answeredLine(an.ID, cmp.Or(e.By.Person, e.By.Name), truncate(an.Answer, 300)))
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
		if keptBy(st, ag, w.now) != "" {
			continue // parked on purpose: a person or its timer brings it back
		}
		k := cmp.Or(ag.Session, ag.Name)
		stopped[k] = true
		if !w.stopped[k] {
			hint := resumeHint(ag)
			switch on := parkedOn(waitsOf(st.Records, ag), w.cfg.Guide.Person); {
			case ag.Import.Pending(w.now):
				hint = importStatus(ag, w.now)
			case on != "":
				hint = fmt.Sprintf("parked on a person: %s; their answer resumes it", on)
			case resumedForTask(ag):
				hint = fmt.Sprintf("ended on a wait again after its resume at %s on %s: %s", clock(w.now, ag.ResumedWait), ag.ResumedOn, hint)
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
		if !ag.Import.Pending(w.now) {
			continue
		}
		k := cmp.Or(ag.Session, ag.Name)
		waits[k] = ag.Import.Since
		if !w.waits[k].Equal(ag.Import.Since) {
			now = append(now, fmt.Sprintf("%q (%s)", ag.Name, importStatus(ag, w.now)))
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

// supervisorGone says once, and notifies, when the recorded supervisor's
// CLI stayed gone past the restart grace with no relay open: its grant rule
// holds, so every claim waits for a successor. The notification repeats
// after notify.repeat while the gap lasts; a supervisor back is one line.
// It reports whether a supervisor runs (a restarting one does not: its
// watch restarts with it).
func (w *watcher) supervisorGone(ctx context.Context, st *state.State, sessions []*claude.Session) bool {
	s := st.Supervisor
	sv := readSupervision(st, sessions, w.now, w.cfg.Supervisor.RestartGrace.Duration)
	var key string
	if s != nil {
		key = s.Name + "@" + s.Since.UTC().Format(time.RFC3339)
	}
	if sv.live {
		w.stand.liveTerm, w.stand.liveAt = key, w.now
		w.upAgain(supervisorRole, s.Party)
	}
	if !sv.down() || relayPending(st, st.SupervisorRole(), w.now) || w.firstTurn(ctx, st, s.Party) {
		switch {
		case s == nil:
			w.gap = ""
		case sv.live && w.gap != "":
			w.gap = ""
			w.emitNow("supervisor", "SUPERVISOR BACK: %q supervises since %s", s.Name, clock(w.now, s.Since))
		}
		return sv.live
	}
	successor := ""
	if w.standby && !w.reopenAfterAppStart(ctx, st, sv.gone, key) {
		successor = w.standIn(ctx, supervisorRole, st, s.Party, key)
	}
	l := fmt.Sprintf("SUPERVISOR GONE: %q (supervising since %s) is gone since %s; claims stay gated until a successor's beekeeper supervisor start (beekeeper handover --prompt)%s",
		s.Name, clock(w.now, s.Since), clock(w.now, sv.gone), successor)
	if w.gap != key {
		w.gap = key
		w.emitNow("supervisor", "%s", l)
	}
	w.notify(ctx, notify.NoSupervisor, key, "beekeeper: no supervisor, claims gated", l)
	return false
}

// quietness reads whether the machine is at a quiet moment, once the
// supervisor's context reached relayAt and this watch did not say its relay
// due yet.
func (w *watcher) quietness(ctx context.Context, st *state.State, sessions []*claude.Session) quietness {
	relayAt := w.cfg.Supervisor.RelayAt
	c := relayContext(st, sessions, w.now, relayAt, w.dues)
	if c == 0 {
		return quietness{}
	}
	return quietness{checked: true, context: c, relayAt: relayAt, busy: w.busyNow(ctx, st)}
}

// busyNow says what keeps the machine from a quiet moment for a relay
// (busyWith), "" when quiet: outside the state lock, since a settling
// merge's installation is read with kubectl.
func (w *watcher) busyNow(ctx context.Context, st *state.State) string {
	holders, err := lease.Dir(w.cfg.LeaseDir).List()
	if err != nil {
		return fmt.Sprintf("the leases cannot be read: %v", err)
	}
	hrs := map[string][]merge.HelmRelease{}
	rolled := func(m state.Merge, lane config.Lane) bool {
		h, ok := hrs[lane.Name]
		if !ok {
			h, _ = w.helmReleases(ctx, lane) // unreadable: not rolled, not quiet
			hrs[lane.Name] = h
		}
		ready, _ := merge.Ready(lane, h, &m, w.now, w.cfg.Merge.Settle.Duration)
		return h != nil && ready
	}
	return busyWith(st, w.cfg, heldMap(holders), w.now, proc.Alive, rolled)
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
		if live || !r.Ended.IsZero() {
			continue
		}
		r.Ended = now.UTC()
		lines = append(lines, fmt.Sprintf("SESSION ENDED: %q, which %s: re-query %s", r.Session.Name, truncate(recordText(*r), 200), r.Issue))
		evs = append(evs, event(watchParty, "session.ended", "%s: %s", r.Session.Name, recordText(*r)))
	}
	return lines, evs
}

// runaways prints one line for each session figure over its threshold in
// metrics.runaway, once per watch: a GitHub budget drain, the same failing
// tool call repeating, a context near its window.
func (w *watcher) runaways(sessions []*claude.Session, t *proc.Table) {
	holders, _ := lease.Dir(w.cfg.LeaseDir).List()
	_, ms := w.sessionMetrics(sessions, t, holders)
	for i, s := range sessions {
		lines := w.runaway(s, ms[i])
		for _, figure := range slices.Sorted(maps.Keys(lines)) {
			key := "runaway " + s.Key() + " " + figure
			if !w.reported[key] {
				w.reported[key], w.dirty = true, true
				w.emitNow(key, "%s", lines[figure])
			}
		}
	}
}

func (w *watcher) staleLeases(ctx context.Context, sessions []*claude.Session) {
	holders, err := lease.Dir(w.cfg.LeaseDir).List()
	if err != nil {
		return
	}
	for _, h := range holders {
		v := w.leaseView(sessions, h)
		key := h.Env + "@" + h.Since
		if v.State == holderGone && !w.reported["lease "+key] {
			w.reported["lease "+key], w.dirty = true, true
			l := fmt.Sprintf("STALE LEASE: %s is held by %q, whose session no longer runs (%s)", h.Env, v.Name, truncate(h.Purpose, 60))
			w.emitNow("lease", "%s", l)
			w.notify(ctx, notify.StaleLease, key, "beekeeper: stale lease "+h.Env, l+"\nbeekeeper lease status "+h.Env)
		}
	}
}
