package cmd

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/notify"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) watchCmd() *cobra.Command {
	var once, notifyDesktop, standby bool
	c := &cobra.Command{
		Use:   "watch",
		Short: "Stay silent until something needs a look, then say it in one line",
		Long: `Poll the machine every watch.interval (30s) and print one line per event,
silent otherwise: made to be the source of a Monitor, so the supervisor
wakes only when something needs a look.

Threshold breaches (RAM, swap, desktop scope, load, memory pressure, tmpfs,
disk, the GitHub budget) and unreadable sources are one line when they
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
and one ENDED line when it ends. A settling merge leaves its lane
once the lane has settled (its release rolled and its HelmReleases Ready,
or no installation to roll), logged as lane.settled, silently. A note or a
timer that falls due, the end of a session with a record (sessions serve)
and a supervisor relay taken or expired are one line each, once: the state keeps that they were reported, so
a second or restarted watch stays silent about them. The
installations' alerts are read every alerts.every and each NEW or RESOLVED
one at or above its installation's floor is a line, a flapping one a single
FLAPPING line (beekeeper alerts watch); only one watch at a time reads them.

Every watch.interval the same installations' Cluster API clusters are read
(one list each of Clusters, KubeadmControlPlanes, MachinePools and
MachineDeployments per installation, within alerts.timeout, in parallel). A
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
claimed, no claim queued and no relay open. It is said once per supervisor,
and again only after a relay is cancelled or expires.

A registered agent whose session's context reaches agents.relayAt gets one
HANDOVER DUE "<agent>" at <n>k: beekeeper agents handover "<agent>" at its
first quiet moment: no tool command of its own running and no gated merge
of its own in flight.

--notify also sends the events that need a person to the desktop's
notification service (org.freedesktop.Notifications on the session bus):
the kinds in notify.kinds, a note or timer falling due (due), the machine
near its OOM line (oom-line), a kernel OOM kill outside a build slot or a
systemd-oomd kill (oom-kill), the GitHub budget under the floor (budget), a
stale lease (stale-lease) and a supervisor whose CLI stayed gone past
supervisor.restartGrace with no relay open (no-supervisor, critical). Each
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
view.

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
				d := &notify.Desktop{}
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
	// standby leaves a running supervisor's events to its watch.
	standby bool
	// gap is the term of the gone supervisor this watch said, until a
	// supervisor is back.
	gap string
	// records are the session records of the last poll: their sessions'
	// ends get the record's line instead of the SESSIONS ended one.
	records []state.Record
	// spare is the standby watch's keep-awake and hand-over memory; table
	// the last poll's process table.
	spare spareWatch
	table *proc.Table
}

func (w *watcher) run(ctx context.Context, once bool) error {
	w.lastPoll = time.Now().Add(-w.cfg.Watch.Interval.Duration)
	p := machine.FindScope()
	if p != "" {
		w.scopeOOM = machine.ReadScope(p).OOMKills
	}
	w.check("noscope", p == "", "no Claude Desktop scope found; watching the machine numbers only")
	var wg sync.WaitGroup
	switch {
	case w.standby:
	case once:
		w.upgradeCycle(ctx)
	default:
		wg.Go(func() { w.watchAlerts(ctx) })
		wg.Go(func() { w.watchUpgrades(ctx) })
	}
	defer wg.Wait()
	tick := time.NewTicker(w.cfg.Watch.Interval.Duration)
	defer tick.Stop()
	for {
		w.poll(ctx)
		if once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// watchAlerts reads the alerts every alerts.every while this watch owns the
// baseline; another watch that owns it is said once, and this one takes over
// when it ends.
func (w *watcher) watchAlerts(ctx context.Context) {
	store := alerts.NewStore(w.cfg.StateDir)
	defer func() { _ = store.Release() }()
	other := 0
	for {
		start := time.Now()
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
		select {
		case <-ctx.Done():
			return
		case <-time.After(max(time.Until(start.Add(w.cfg.Alerts.Every.Duration)), 0)):
		}
	}
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
		reported: map[string]bool{}, active: map[string]condition{}}
	w.spare = spareWatch{send: a.peerSend, sent: map[string]time.Time{}, checked: map[string]bool{}}
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
	if w.notifier != nil && body != "" {
		w.notifier.Notify(ctx, w.now, kind, key, summary, body)
	}
}

// oomLine notifies a breach of the OOM line the poll printed.
func (w *watcher) oomLine(ctx context.Context, line string) {
	w.notify(ctx, notify.OOMLine, "", "beekeeper: the machine is near its OOM line", line+"\nbeekeeper snapshot")
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

func (w *watcher) poll(ctx context.Context) {
	w.now = time.Now()
	since := w.lastPoll
	w.lastPoll = w.now
	th := w.cfg.Watch

	if m, err := machine.ReadMem(); err == nil {
		w.oomLine(ctx, w.check("avail", m.AvailableMiB < th.AvailMinMiB, "LOW RAM: %d MiB available, swap %d MiB", m.AvailableMiB, m.SwapUsedMiB))
		w.oomLine(ctx, w.check("swap", m.SwapUsedMiB > th.SwapMaxMiB, "SWAP: %d of %d MiB used", m.SwapUsedMiB, m.SwapTotalMiB))
	}
	if l, err := machine.ReadLoad(); err == nil {
		w.check("load", l[0] > th.LoadMax, "HIGH LOAD: %.0f", l[0])
	}
	if psi, err := machine.ReadPSIFull60(); err == nil {
		w.oomLine(ctx, w.check("psi", psi > th.PSIMax, "MEMORY PRESSURE: full avg60 %.0f%%", psi))
	}
	if d, err := machine.ReadDisk("/tmp"); err == nil {
		w.check("tmp", d.UsedMiB > th.TmpMaxMiB, "TMPFS /tmp: %d MiB", d.UsedMiB)
	}
	if d, err := machine.ReadDisk("/"); err == nil {
		w.check("disk", d.FreeMiB < th.DiskMinMiB, "LOW DISK: / %d GiB free", d.FreeMiB/1024)
	}
	if p := machine.FindScope(); p != "" {
		s := machine.ReadScope(p)
		w.oomLine(ctx, w.check("scopeanon", s.AnonMiB > th.ScopeAnonMaxMiB, "DESKTOP SCOPE anon: %d MiB", s.AnonMiB))
		w.oomLine(ctx, w.check("scope", s.CurrentMiB > th.ScopeMaxMiB, "DESKTOP SCOPE near its cap: %d MiB RAM + %d MiB swap of %s", s.CurrentMiB, s.SwapMiB, s.Max))
		if s.OOMKills != w.scopeOOM {
			w.emitNow("scopeoom", "OOM KILL in the desktop scope: oom_kill %d -> %d", w.scopeOOM, s.OOMKills)
			w.scopeOOM = s.OOMKills
		}
	}

	t, err := proc.Read()
	if err != nil {
		w.emit("proc", "cannot read the process table: %v", err)
		return
	}
	w.clear("proc")
	w.table = t
	sessions := claude.Discover(w.cfg, t, w.now)
	w.kills(ctx, since, sessions, t)
	w.pending(ctx, sessions)
	w.sessionChanges(sessions)
	w.staleLeases(ctx, sessions)
	w.runaways(sessions, t)
	w.stalls()
	w.settled(ctx)

	if w.now.Sub(w.lastBudget) >= th.BudgetEvery.Duration {
		w.lastBudget = w.now
		b, err := w.probeBudget(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
		case err != nil:
			w.emit("budget-error", "GitHub budget unknown: %v", err)
		default:
			w.clear("budget-error")
			l := w.check("budget", b.Remaining < w.cfg.GitHub.Floor, "GITHUB BUDGET %d of %d: hold GitHub work until %s",
				b.Remaining, b.Limit, b.Reset.Local().Format("15:04"))
			w.notify(ctx, notify.Budget, "", "beekeeper: GitHub budget under the floor", l+"\nbeekeeper budget")
		}
	}
	w.saveMark()
	if w.notifier != nil {
		w.notifier.Flush(ctx, w.now)
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
	w.mu.Lock()
	var over []string
	for k := range w.active {
		if strings.HasPrefix(k, "stall:") && !stalled[k] {
			over = append(over, k)
		}
	}
	w.mu.Unlock()
	for _, k := range over {
		w.clear(k)
	}
}

// settled drops the settling merges whose lane has settled, so lanes shows
// the lane free before its next merge starts: a lane with no installation
// has nothing to roll, and one whose release rolled and whose HelmReleases
// are Ready is done. A lane past merge.settleTimeout is left to lanes clear.
func (w *watcher) settled(ctx context.Context) {
	st, err := w.store.Read()
	if err != nil {
		return
	}
	hrs := map[string][]merge.HelmRelease{}
	done := map[string]string{}
	for _, m := range st.Merges {
		if m.Phase != state.Settling || w.now.Sub(m.Finished) > w.cfg.Merge.SettleTimeout.Duration {
			continue
		}
		lane, ok := w.cfg.LaneNamed(m.Lane)
		if !ok || lane.Installation == "" {
			done[m.Key()] = "no installation to roll"
			continue
		}
		h, read := hrs[lane.Name]
		if !read {
			h, err = readHelmReleases(ctx, lane)
			if err != nil {
				continue // unreadable: not settled
			}
			hrs[lane.Name] = h
		}
		if ready, _ := merge.Ready(lane, h, &m, w.now, w.cfg.Merge.Settle.Duration); ready {
			done[m.Key()] = "rolled, HelmReleases of " + lane.Installation + " Ready"
		}
	}
	if len(done) == 0 {
		return
	}
	_ = w.store.Update(func(st *state.State) ([]state.Event, error) {
		var ev []state.Event
		st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool {
			why, ok := done[m.Key()]
			if !ok || m.Phase != state.Settling {
				return false
			}
			ev = append(ev, event(watchParty, "lane.settled", "%s: %s %s", m.Lane, m.Key(), why))
			return true
		})
		return ev, nil
	})
}

// commandTimeout bounds one journalctl or docker call of a poll: a wedged
// daemon must not stall the watch or its shutdown.
const commandTimeout = 20 * time.Second

func (w *watcher) kills(ctx context.Context, since time.Time, sessions []*claude.Session, t *proc.Table) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	kills, err := machine.OOMKills(ctx, since.Add(-2*time.Second))
	if err != nil {
		if ctx.Err() != nil {
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
	w.notifyKills(ctx, fresh)
	if lines, err := machine.OomdKills(ctx, since.Add(-2*time.Second)); err == nil {
		for _, l := range lines {
			if !w.seenKills[l] {
				w.seenKills[l] = true
				w.emitNow("oomd", "SYSTEMD-OOMD: %s", truncate(l, 200))
				w.notify(ctx, notify.OOMKill, l, "beekeeper: systemd-oomd killed a unit", truncate(l, 200)+"\nbeekeeper snapshot")
			}
		}
	}
}

// notifyKills sends the kernel OOM kills no other watch has claimed, as one
// notification; a build slot's cap killing its own command is left out,
// its session sees the exit.
func (w *watcher) notifyKills(ctx context.Context, kills []oomKill) {
	if w.notifier == nil {
		return
	}
	var keys []string
	byKey := map[string]oomKill{}
	for _, k := range kills {
		if strings.Contains(k.Memcg, "memcap") {
			continue
		}
		key := fmt.Sprintf("%d@%d", k.PID, k.At.Unix())
		keys = append(keys, key)
		byKey[key] = k
	}
	if len(keys) == 0 {
		return
	}
	var claimed []oomKill
	for _, key := range w.notifier.Claim(ctx, w.now, notify.OOMKill, keys...) {
		claimed = append(claimed, byKey[key])
	}
	if len(claimed) > 0 {
		w.notifier.Send(ctx, w.now, notify.OOMKill, "beekeeper: kernel OOM kill", strings.Join(groupKills(claimed), "\n")+"\nbeekeeper snapshot")
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
	var started, ended, restarted []string
	for k, s := range cur {
		prev, ok := w.sessions[k]
		switch {
		case !ok:
			started = append(started, fmt.Sprintf("%q", s.Name))
		case prev.PID != s.PID:
			restarted = append(restarted, fmt.Sprintf("%q", s.Name))
		}
	}
	for k, s := range w.sessions {
		_, ok := cur[k]
		recorded := slices.ContainsFunc(w.records, func(r state.Record) bool { return r.Session.Is(s.Party()) })
		if !ok && !recorded {
			ended = append(ended, fmt.Sprintf("%q", s.Name))
		}
	}
	for _, l := range [][]string{started, ended, restarted} {
		slices.Sort(l)
	}
	if len(started) > 0 {
		w.emitNow("sessions", "SESSIONS started: %s", strings.Join(started, ", "))
	}
	if len(ended) > 0 {
		w.emitNow("sessions", "SESSIONS ended: %s", strings.Join(ended, ", "))
	}
	if len(restarted) > 0 {
		w.emitNow("sessions", "SESSIONS restarted: %s", strings.Join(restarted, ", "))
	}
	w.sessions = cur
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
		w.tendSpare(ctx, st, sessions)
	}
	if w.standby && supervised {
		w.resumeRestarted(ctx, st, sessions)
		return // the supervisor's watch reports them
	}
	q := w.quietness(ctx, st, sessions)
	w.handoversDue(st, sessions)
	fire := func(st *state.State) ([]string, []state.Event, bool) {
		seen, ce := observeCLI(st, sessions, w.now)
		lines, evs := firePending(st, sessions, w.now)
		for _, e := range ce {
			lines = append(lines, fmt.Sprintf("SUPERVISOR RESTARTED: %q, %s; it keeps the role", e.By.Name, e.Detail))
		}
		evs = append(evs, ce...)
		rl, re := fireRelay(st, w.now)
		dl, de := fireRelayDue(st, q, w.now)
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
	for _, d := range due {
		w.notify(ctx, notify.Due, d.key, d.summary, d.body)
	}
}

// dueItem is a note or timer this poll reported due.
type dueItem struct{ key, summary, body string }

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
	if !sv.down() || st.Relay.Open(w.now) {
		switch {
		case s == nil:
			w.gap = ""
		case sv.live && w.gap != "":
			w.gap = ""
			w.emitNow("supervisor", "SUPERVISOR BACK: %q supervises since %s", s.Name, clock(w.now, s.Since))
		}
		return sv.live
	}
	key := s.Name + "@" + s.Since.UTC().Format(time.RFC3339)
	spare := ""
	if w.standby {
		spare = w.handOver(ctx, st, sessions, key)
		if st.Spare == nil || strings.Contains(spare, "no running CLI") {
			w.reopenAfterAppStart(ctx, st, sv.gone, key)
		}
	}
	l := fmt.Sprintf("SUPERVISOR GONE: %q (supervising since %s) is gone since %s; claims stay gated until a successor's beekeeper supervisor start (beekeeper handover --prompt)%s",
		s.Name, clock(w.now, s.Since), clock(w.now, sv.gone), spare)
	if w.gap != key {
		w.gap = key
		w.emitNow("supervisor", "%s", l)
	}
	w.notify(ctx, notify.NoSupervisor, key, "beekeeper: no supervisor, claims gated", l)
	return false
}

// quietness reads whether the machine is at a quiet moment, once the
// supervisor's context reached relayAt: outside the state lock, since a
// settling merge's installation is read with kubectl.
func (w *watcher) quietness(ctx context.Context, st *state.State, sessions []*claude.Session) quietness {
	c := relayContext(st, sessions, w.now, w.cfg.Supervisor.RelayAt)
	if c == 0 {
		return quietness{}
	}
	holders, err := lease.Dir(w.cfg.LeaseDir).List()
	if err != nil {
		return quietness{checked: true, context: c, busy: fmt.Sprintf("the leases cannot be read: %v", err)}
	}
	hrs := map[string][]merge.HelmRelease{}
	rolled := func(m state.Merge, lane config.Lane) bool {
		h, ok := hrs[lane.Name]
		if !ok {
			h, _ = readHelmReleases(ctx, lane) // unreadable: not rolled, not quiet
			hrs[lane.Name] = h
		}
		ready, _ := merge.Ready(lane, h, &m, w.now, w.cfg.Merge.Settle.Duration)
		return h != nil && ready
	}
	return quietness{checked: true, context: c, busy: busyWith(st, w.cfg, heldMap(holders), w.now, proc.Alive, rolled)}
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
		if !state.Due(t.Due, t.Fired, now) {
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
