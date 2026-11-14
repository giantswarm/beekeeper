package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/notify"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/upgrade"
	"github.com/giantswarm/beekeeper/pkg/project"
)

func (a *app) watchCmd() *cobra.Command {
	var once, notifyDesktop, standby bool
	var tick time.Duration
	c := &cobra.Command{
		Use:   watchName,
		Short: "Stay silent until something needs a look, then say it in one line",
		Long: `Poll the machine every watch.interval (30s) and print one line per event,
silent otherwise: made to be the source of a Monitor, so the supervisor
wakes only when something needs a look.

Routine lines (doctor chores, session changes, load/pressure recovery and
other teams' notify alerts) are retained as individual durable events and
printed together every watch.batchInterval (5m). Each BATCH is one physical
stdout line with a JSON array of timestamped items, so a line-oriented
Monitor receives one event. Empty ticks print nothing. --tick <duration>
overrides that interval; a negative watch.batchInterval disables batching.
--once prints immediately. Action lines, including unknown event types,
OOM kills, memory thresholds, pages, due timers, reports, runaways, leases
and grants, never wait for the batch. Parks needing a person's answer and
resumes needing action remain immediate: they have no recorded provenance
proving that they merely echo an earlier supervisor command.
Buffered items are already in beekeeper log under their watch.* verb,
including if the watch exits or restarts before its next tick.

Threshold breaches (RAM, swap, desktop scope, load, CPU and memory
pressure, tmpfs, disk, the GitHub budget) and unreadable sources are one line when they
start and one ENDED line when they end, never repeated while they last.
Under watch.diskMinMiB free on / the watch says LOW DISK, under
watch.diskCriticalMiB DISK NEARLY FULL, and while / would run full within
watch.diskFillWithin (2h) at the rate its free space fell over the last ten
minutes DISK FILLING, naming the commands and sessions that wrote most and
how much of the loss no process it can read accounts for.
OOM kills are never folded away: every poll reports every kill since the
last one, grouped by whose limit they hit; a cap kill whose scope no
run.start names says its cap is unknown. A kill in a test run's scope
(memcap-test-…, MEMCAP_TEST=1) is one quiet "test kill" line, never a
KERNEL OOM. Sessions that start, end or
restart are reported, and so is a lease whose holder is gone. A lease held
past the first duration in its purpose (or leaseRound, default 1h) is one
LEASE HELD PAST ROUND line naming its holder, since when and its overrun.
A run
holding a build slot past memcap.holdBound (30m) is one SLOT HELD LONG
line naming the slot, its holder and since when, and one ENDED line once
the slot is free. A session
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
merge at the next poll. Startup, a detected suspend, or a long gap between
checks gives waits a fresh merge.hungAfter grace period before they can
be ended as hung. A
registered agent with a task and no running CLI is one AGENTS STOPPED
line with how to resume it, unless it is kept (agents keep, or a timer
that wakes it). A positive capacity.ceiling reached is one CAPACITY FULL
line with the headroom (beekeeper capacity) and one ENDED line when the
count recovers; capacity.ceiling of 0 suppresses CAPACITY FULL. When capacity.usage.command or capacity.providers is configured, the watch refreshes each
subscription's 5-hour and 7-day usage windows every usage.every
(default 5m, and once resetsAt passes), prints one USAGE line (with each
provider's windows and verdict and the capacity.choices selection when
capacity.providers is configured, else every capacity.usage.reportEvery,
default 30m, negative turns it off, and once on watch --once), and once a
5-hour window reaches usage.highPercent (default 95%) emits USAGE HIGH once
(and ENDED USAGE HIGH once reset), tells each running busy worker on that
provider once per window episode to finish its step and park, and holds
auto-resume until fiveHour.resetsAt. A reopen that waits to show an agent in the desktop (the
window has the person's focus) is one IMPORT WAITS line per wait, with
what it waits for and until when. A settling merge leaves its lane
once the lane has settled (its release rolled and its HelmReleases Ready,
or no installation to roll), logged as lane.settled, silently, however
late; one not settled past merge.settleTimeout is one LANE STUCK line
with what the lane waits for and one ENDED line when it ends. A note or a
timer that falls due, the end of a session with a record (sessions serve)
and a supervisor relay taken or expired are one line each, once: the state keeps that they were reported, so
a second or restarted watch stays silent about them. An open note for
someone, not a login, is overtaken once every issue and pull
request it names with note add --ref is closed or merged (one GraphQL read
a poll, none under the budget floor; a note with no --ref never is, guide
watch names it once its filing session is archived): one NOTE OVERTAKEN
line with the reason,
the note closed and note.overtaken logged; --once names each one it would
close and writes nothing. The
installations' alerts are read every alerts.every and each NEW or RESOLVED
one at or above its installation's floor is a line, a flapping one a single
FLAPPING line, and a reading that misses alerts holds them until the next
one confirms (beekeeper alerts watch); only one watch at a time reads them.
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
them all, in parallel, with one retry and a deadline per attempt of at least
two minutes or alerts.timeout if longer). The readings are shared
(upgrades.json): a second watch, snapshot and ui use a fresh one instead of
reading again. A
cluster whose release changes begins an upgrade: its release label differs
from the one cluster-api-events recorded, cluster-api-events marks it
upgrading, or its scheduled upgrade is due. It ends once none of that holds
and its control plane and node pools have rolled; a marked one once the
cluster reports the release cluster-api-events recorded and has rolled out
for an hour from its start stamp, or that start is a day ago; a rollout
after the mark cleared a day after its hold began. While it runs, the
installation is held (hold upgrade:<installation>/<cluster>): the gate
refuses the merges of its lanes and lease claims of its name are refused.
"UPGRADE <installation>/<cluster> <from> → <to>" is said when it begins and
"UPGRADE ENDED …" when it ends, once each; an unreadable installation is one
line naming the cause and deadline and keeps its holds. Held upgrades name
the number of consecutive ticks their reader has failed. The installations whose kube context is not in the
kubeconfig kubectl reads are not read, keep their holds and alert baselines,
and are named once per watch in one line each for UPGRADES and ALERTS.

The supervisor is the supervisor loop, no session. Once the loop has not
run for two minutes every watch says SUPERVISOR LOOP DOWN once, and
SUPERVISOR LOOP BACK when it runs again; on Linux the standby watch
restarts its systemd service meanwhile (see --standby).

Every poll but --once's runs beekeeper doctor in the background (its last
run still going, the poll skips it): one DOCTOR line per agent it took off
the roster, desktop session it archived or retitled, fault it fixed or
note it filed, and one DOCTOR FAULT line while a known fault lasts. Every
doctor.freeEvery (1h) one watch frees what dead sessions and dead go
commands left in the tmpfs /tmp (beekeeper free --apply --only
session-dirs,go-build): one FREED line when it freed something, FREE
FAILED while it fails; the TMPFS line says when it ran last. Above the
tmpfs warning threshold it also names the three largest entries, their
sizes, and the commands and worker sessions referencing them when known.

A running beekeeper watch, this one included, whose binary self-update
replaced keeps the code it started with: one WATCH STALE line names the
watch, the version it runs and the one installed, and that a re-arm (a
restart) picks it up; one ENDED line follows once it exits. --once says
the stale watches it finds as well. Once beekeeper install or self-update
asks (restart.json in the state directory), a watch of the replaced binary
restarts on the installed one: after its current poll it stops as on
SIGTERM, waits for every run it started, so a save under way ends first,
says WATCH RESTART and re-executes the installed binary with its own
command line, the same pid and output; it restarts once per request, and
an alerts or guide watch keeps its owner's re-arm.

A registered agent whose session's context reaches agents.relayAt gets one
HANDOVER DUE "<agent>" at <n>k: beekeeper agents handover "<agent>" at its
first quiet moment: no tool command of its own running and no gated merge
of its own in flight. An agent in its task's last step (its sessions serve
record waits on the report or says the merge landed, or the gate saw its
merge land) is HANDOVER HELD instead, once, for agents.lastStepGrace (30m)
from that evidence or until its context reaches agents.lastStepCeiling:
its report is expected before a hand-over would pay, and an agent that
reported done is never handed over. Past the grace or the ceiling the
HANDOVER DUE line says how long it was held.

--notify also sends the events that need a person to the desktop's
notification service (org.freedesktop.Notifications on the session bus):
the kinds in notify.kinds, a note or timer falling due (due), the GitHub
budget under the floor (budget), a stale lease (stale-lease), a
supervisor whose CLI stayed gone past supervisor.restartGrace with no relay
open (no-supervisor, critical) and an unowned page (page-unowned, critical). The machine's lines (memory, swap, OOMD
IMMINENT, OOM kills, load, processes) never notify: the supervisor acts on
them, and its watch says them. Each
event is one notification however many watches notify: the first to claim
it in notify.json sends it. A lasting condition and the supervisor loop
down notify again after notify.repeat (30m); notify.quietHours hold
everything but a critical one and send what they held as one notification
when they end. Nothing routine notifies. With no notification service the
watch runs on, prints its lines and says so once.

With sink.repo set, a running watch (not --once) dispatches sink.workflow
for each event of sink.verbs within one interval, with the inputs verb,
subject (owner/repo#n) and time only: plan.grilling, plan.ready,
session.serve, board.claim and merged. sink.json in the state directory
claims each event once across the machine's watches; a dispatch is logged
(sink.dispatched), a failed one said (SINK FAILED, sink.failed) and tried
again by the next poll.

--standby is for a watch that runs beside the supervisor loop (the
beekeeper-notify user unit): while the loop runs it leaves the notes,
timers, session records and relays to the loop's watch, and it never reads
the alerts, so it takes nothing from the loop's view. While the loop does
not run, it starts the beekeeper-supervisor-loop systemd service again
(Linux only: on macOS launchd restarts a failed loop itself), at most once
a minute and never during a stop (SUPERVISOR LOOP RESTARTED); it never
starts a session for the supervisor. A scratch configuration's watch
restarts no service. It reads the upgrades and sets and lifts their holds
only while no loop's watch has begun an upgrade cycle within five
watch.interval (upgrades-watch.json), and says so once. It relays a guide
that is still over its relayAt guide.relayGrace (30m) after its relay due,
with no relay open: it starts the successor as the holder's own relay does
(GUIDE RELAYED), and the successor's start takes the role.

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
sessions of the processes started since the last sample (the watch's own,
its alert reads among them, as "beekeeper watch"); more than
watch.stackMax (3) copies of one command line from the same place in the
process tree, each running over a minute, are one STACKED line with the
count, the oldest's age, its parent and its session. More than
watch.toolProcsMax (1000) processes of the CLIs in watch.tools (kubectl,
helm, tsh, gh, flux, devctl) running machine-wide are one LOAD line with
their commands and the sessions that run them. A parent-child chain of one
CLI longer than watch.toolProcsMax is named as a possible recursion. Each ends with an ENDED
line; a negative threshold turns it off. A printed command line
keeps no value: the program, its subcommands and the flag names.

What a watch has said is kept per caller (seen.watch.<caller>.json): a
restarted watch of the same session says no open condition, runaway or
stale lease again, only its end or what is new. Runs until killed. --once
polls once, keeps no mark and says every condition it finds; it fires no
timer (no wake, no command, no close or re-arm), each one due a TIMER line
marked that --once writes nothing; a read that
fails (the state, a source, GitHub) is a line and makes it exit 1, so a
silent --once with exit 0 means nothing changed. The notes' read is said
on the NOTE path: NOTE REFS UNREADABLE when GitHub fails it, NOTE REFS
NOT READ while the budget is under its floor and NOTE REFS UNANSWERED
for an issue or pull request GitHub did not answer, which keeps its notes
open.

In the agent sandbox, which closes the person's kubeconfig and Teleport
login, the user bus and the notification service, the host's broker runs
the watch as the session, in a scope of its own, and streams its lines
back for as long as the sandboxed command runs: a Monitor reads them as
it reads a watch on the host, and stopping it stops the watch.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if os.Getenv(restartedEnv) == "" {
				ownScope("watch", brokeredWatchCap)
			}
			if cmd.Flags().Changed("tick") && tick <= 0 {
				return fmt.Errorf("--tick must be positive")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			var restart *restartWatch
			if !once {
				restart = newRestartWatch(a.cfg.StateDir)
			}
			for {
				w := a.newWatcher(standby, !once)
				w.restart = restart
				if tick > 0 {
					w.batchEvery = tick
				}
				if standby && a.explicitConfig() == "" {
					// The standby restarts the machine's own loop service
					// while the loop is down; a scratch configuration's
					// watch restarts none, nor does any other watch.
					w.stand.loopUnit = launchedLoop()
				}
				err := a.runWatcher(ctx, w, once, notifyDesktop)
				if !errors.Is(err, errRestart) {
					return err
				}
				w.emitNow("restart", "WATCH RESTART: %s asked this watch onto the installed %s; re-executing it", w.restartFor.Binary, project.Name)
				if err := restart.exec(w.restartFor); err != nil {
					w.emitNow("restart", "WATCH RESTART FAILED: the installed %s does not start (%v): carrying on under %s", w.restartFor.Binary, err, project.Version())
				}
			}
		},
	}
	c.Flags().BoolVar(&once, "once", false, "poll once and exit")
	c.Flags().BoolVar(&notifyDesktop, "notify", false, "send the events that need a person to the desktop too (notify.kinds)")
	c.Flags().BoolVar(&standby, "standby", false, "while a supervisor runs, leave its events to its watch and never read the alerts")
	c.Flags().DurationVar(&tick, "tick", 0, "Batch routine events at this interval; action events remain immediate")
	return c
}

// runWatcher runs w until ctx ends or an install asks it to restart, with
// its desktop notifier (--notify) open for that run.
func (a *app) runWatcher(ctx context.Context, w *watcher, once, notifyDesktop bool) error {
	if notifyDesktop {
		d := plat.NewNotifier()
		defer func() { _ = d.Close() }()
		w.notifier = notify.New(a.cfg.Notify.Policy(), a.cfg.StateDir, d, func(l string) { w.emitNow("notify", "%s", l) })
	}
	return w.run(ctx, once)
}

// transcriptFresh is how long a watch takes an unchanged transcript's last
// read for its figures: an idle session's last hour ages that much before
// it is read again.
const transcriptFresh = 5 * time.Minute

// newWatcher is a watch; one that keeps a mark resumes its caller's last
// watch (seen.watch.<caller>.json): it says no open condition or event
// again. --once and a watch outside a Claude session keep none.
func (a *app) newWatcher(standby, keep bool) *watcher {
	w := &watcher{a: a, standby: standby, last: map[string]time.Time{}, seenKills: map[string]bool{},
		reported: map[string]bool{}, active: map[string]condition{}, chores: keep, retitled: map[string]time.Time{}, doctored: map[string]bool{},
		upgrades: upgrade.Readings{}}
	if keep {
		// A poll reads every session's transcript (RUNAWAY): an idle
		// session's read stays good for a few polls.
		a.transcripts = claude.NewTranscriptCache(transcriptFresh)
	}
	w.stand = standbyWatch{send: a.peerSend, succeed: a.succeedFromWatch, revive: a.reviveFromWatch, turning: unitsTurning, reopening: unitsReopening, importRow: a.importRowFromWatch}
	if me, err := a.caller(); keep && err == nil {
		w.markFile = "seen.watch." + fileKey(me) + ".json"
		var m watchMark
		if found, err := a.store.ReadFile(w.markFile, &m); err == nil && found {
			maps.Copy(w.active, m.Conditions)
			maps.Copy(w.reported, m.Reported)
			w.feedID = m.FeedID
			w.usageReported = m.UsageReported
		}
	}
	return w
}

// watchTeleport says the Teleport login's trouble, one line when it starts and
// one ENDED line when it ends: about to expire, expired, or its keeper's
// renewal failed.
func watchTeleport(w *watcher, ctx context.Context) {
	v := w.a.readTeleport(ctx)
	if v == nil {
		return
	}
	for _, k := range teleportKeys {
		if k != v.Key {
			w.clear(k)
		}
	}
	if v.Key != "" {
		w.emit(v.Key, "%s", v.line(clk.Now()))
	}
}

// watchBudget probes the GitHub budget and says when it is under the floor and
// when GitHub refuses GraphQL calls, with the callers drawing on it.
func watchBudget(w *watcher, ctx context.Context, now time.Time) {
	b, err := readBudget(w.a, ctx)
	if ctx.Err() == nil {
		held := err != nil || b.Remaining < w.a.cfg.GitHub.Floor || b.GraphQL == nil || b.GraphQL.Err != "" || b.GraphQL.Remaining < w.a.cfg.GitHub.Floor || b.GraphQL.Blocks(now)
		w.publishBudget(held)
	}
	switch {
	case err != nil && ctx.Err() != nil:
	case err != nil:
		if l := w.fail("budget-error", "GitHub budget unknown: %v", err); l != "" {
			w.notifyAt(ctx, now, notify.Budget, "", "beekeeper: GitHub budget unknown", l+"\nbeekeeper budget")
		}
	default:
		w.clear("budget-error")
		if l := w.check("budget", b.Remaining < w.a.cfg.GitHub.Floor, "GITHUB BUDGET %d of %d: hold GitHub work until %s",
			b.Remaining, b.Limit, b.Reset.Local().Format("15:04")); l != "" {
			w.notifyAt(ctx, now, notify.Budget, "", "beekeeper: GitHub budget under the floor", l+"\nbeekeeper budget")
		}
		graphqlLow := b.GraphQL != nil && !b.GraphQL.Blocks(now) && b.GraphQL.Remaining < w.a.cfg.GitHub.Floor
		var gqlRemaining, gqlLimit int
		var gqlReset string
		if b.GraphQL != nil {
			gqlRemaining, gqlLimit, gqlReset = b.GraphQL.Remaining, b.GraphQL.Limit, b.GraphQL.Reset.Local().Format("15:04")
		}
		if l := w.check("graphql-budget", graphqlLow, "GITHUB GRAPHQL BUDGET %d of %d: hold GitHub work until %s",
			gqlRemaining, gqlLimit, gqlReset); l != "" {
			w.notifyAt(ctx, now, notify.Budget, "", "beekeeper: GitHub GraphQL budget under the floor", l+"\nbeekeeper budget")
		}
		refused := b.GraphQL.Blocks(now)
		var callers string
		if refused {
			callers = w.callersText()
			if recorded, err := githubWindowCallers(w.a, b.GraphQL); err != nil {
				callers = "window callers unknown (" + err.Error() + "); live: " + callers
			} else if len(recorded) > 0 {
				callers = "window: " + githubCallersText(recorded) + "; live: " + callers
			}
		}
		if l := w.check("graphql", refused, "%s; callers: %s", graphqlText(w.a, b.GraphQL), callers); l != "" {
			w.notifyAt(ctx, now, notify.Budget, "", "beekeeper: GitHub refuses GraphQL", l+"\nbeekeeper budget")
		}
	}
}

// watchCallersText names the gh and devctl processes drawing on GitHub and the
// sessions they run under, the longest-running first.
func watchCallersText(w *watcher) string {
	sessions, t, err := w.a.sessions()
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	ps := githubCallers(w.a, t, sessions)
	if len(ps) == 0 {
		return "no gh or devctl process now"
	}
	var out []string
	for _, p := range ps[:min(len(ps), 5)] {
		out = append(out, fmt.Sprintf("%s (%s, %s)", truncate(p.Args, 50), cmp.Or(p.Session, noSession), dur(p.Elapsed)))
	}
	if len(ps) > 5 {
		out = append(out, fmt.Sprintf("%d more", len(ps)-5))
	}
	return strings.Join(out, ", ")
}

// watchStalls says each stalled lane, one LANE STALLED line per lane and waiting
// merge when the stall starts and one ENDED line when it ends.
func watchStalls(w *watcher) {
	st, err := w.a.readSnapshot()
	if err != nil {
		return
	}
	stalled := map[string]bool{}
	for _, v := range w.a.laneViews(st) {
		if v.Stall != nil {
			key := "stall:" + v.Name + ":" + v.Stall.Merge.Key()
			stalled[key] = true
			w.emit(key, "LANE STALLED %s: %s", v.Name, w.a.stallText(*v.Stall))
		}
	}
	w.clearMissing("stall:", stalled)
}

// watchHungMerges ends the devctl of each running merge that has outlived its
// pull request (endHung), one MERGE HUNG line each; the next poll records
// it (lostMerges), or its gate does.
func watchHungMerges(w *watcher, ctx context.Context, now time.Time) {
	// GitHub and persisted merge timestamps have no monotonic reading.
	// Never use their wall-clock age alone to kill a wait after a suspend.
	// The wall/monotonic difference detects suspend on platforms whose
	// monotonic clock stops asleep; a long gap also covers other platforms
	// and clocks without monotonic readings.
	gap := max(5*time.Minute, 2*slowReads*w.a.cfg.Watch.Interval.Duration)
	wallElapsed := now.Round(0).Sub(w.lastHungCheck.Round(0))
	elapsed := now.Sub(w.lastHungCheck)
	if w.lastHungCheck.IsZero() || wallElapsed-elapsed > time.Second || wallElapsed > gap || wallElapsed < 0 {
		w.hungGrace = now
	}
	w.lastHungCheck = now
	if now.Sub(w.hungGrace) <= w.a.cfg.Merge.HungAfter.Duration {
		return
	}
	for _, h := range w.a.endHung(ctx, watchParty, now, w.a.cfg.Merge.HungAfter.Duration, func(m state.Merge) bool {
		return now.Sub(m.Started) > w.a.cfg.Merge.HungAfter.Duration
	}) {
		w.emitNow("lanes", "MERGE HUNG: %s in lane %s by %q: %s, its devctl (pid %d) still ran: ended, the lane records it next",
			h.merge.Key(), h.merge.Lane, h.merge.By.Name, w.a.pullText(h.pull), h.merge.Child)
	}
}

// commandTimeout bounds one journalctl or docker call of a poll: a wedged
// daemon must not stall the watch or its shutdown.
const commandTimeout = 20 * time.Second

func watchKills(w *watcher, ctx context.Context, since time.Time, sessions []*claude.Session, t *proc.Table) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	kills, err := plat.Machine.OOMKills(ctx, since.Add(-2*time.Second))
	if err != nil {
		if ctx.Err() != nil || w.unavailable(secOOM, err) {
			return
		}
		w.fail("journal", "cannot read the kernel journal: %v", err)
		return
	}
	w.clear("journal")
	var fresh []oomKill
	var clusters []machine.Cluster
	runs := &runIndex{store: w.a.store}
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

// launchedLoop is the loop's service on plat's launcher, nil where this
// machine has none.
func launchedLoop() *loopService {
	if !plat.Launcher.Available() {
		return nil
	}
	return &loopService{
		state: func(ctx context.Context) string { return plat.Launcher.State(ctx, loopUnit) },
		start: func(ctx context.Context) error { return plat.Launcher.Resume(ctx, loopUnit) },
	}
}

// watchRunaways prints one line for each session figure over its threshold in
// metrics.runaway, once per watch: a GitHub budget drain, the same failing
// tool call repeating, a context near its window.
func watchRunaways(w *watcher, sessions []*claude.Session, t *proc.Table) {
	holders, _ := lease.Dir(w.a.cfg.LeaseDir).List()
	_, ms := w.a.sessionMetrics(sessions, t, holders)
	for i, s := range sessions {
		lines := w.a.runaway(s, ms[i])
		for _, figure := range slices.Sorted(maps.Keys(lines)) {
			key := "runaway " + s.Key() + " " + figure
			if !w.reported[key] {
				w.reported[key], w.dirty = true, true
				w.emitNow(key, "%s", lines[figure])
			}
		}
	}
}

func watchStaleLeases(w *watcher, ctx context.Context, sessions []*claude.Session) {
	holders, err := lease.Dir(w.a.cfg.LeaseDir).List()
	if err != nil {
		return
	}
	found := map[string]bool{}
	for _, h := range holders {
		if h.Damaged != "" {
			found["lease-damaged:"+h.Env] = true
			w.emit("lease-damaged:"+h.Env, "DAMAGED LEASE: %s's holder record cannot be read (%s): it stays held until beekeeper lease release %s --force", h.Env, h.Damaged, h.Env)
			continue
		}
		v := w.a.leaseView(sessions, h)
		key := h.Env + "@" + h.Since
		if overrun := w.a.leaseOverrun(h); overrun != "" {
			found["lease-round:"+key] = true
			w.emit("lease-round:"+key, "LEASE HELD PAST ROUND: %s held by %q since %s: %s%s", h.Env, v.Name, clock(w.a.now, h.SinceTime()), oneLine(h.Purpose), overrun)
		}
		if v.State == holderGone && !w.reported["lease "+key] {
			w.reported["lease "+key], w.dirty = true, true
			l := fmt.Sprintf("STALE LEASE: %s is held by %q, whose session no longer runs (%s)", h.Env, v.Name, truncate(h.Purpose, 60))
			w.emitNow("lease", "%s", l)
			w.notify(ctx, notify.StaleLease, key, "beekeeper: stale lease "+h.Env, l+"\nbeekeeper lease status "+h.Env)
		}
	}
	w.clearMissing("lease-round:", found)
	w.clearMissing("lease-damaged:", found)
}
