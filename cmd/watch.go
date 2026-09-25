package cmd

import (
	"context"
	"fmt"
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
disk, the GitHub budget) repeat at most every watch.repeat (10m) per kind.
OOM kills are never folded away: every poll reports every kill since the
last one, grouped by whose limit they hit. Sessions that start, end or
restart are reported, and so is a lease whose holder is gone. A note or a
timer that falls due, the end of a session with a record (sessions serve)
and a supervisor relay taken or expired are one line each, once: the state keeps that they were reported, so
a second or restarted watch stays silent about them. The
installations' alerts are read every alerts.every and each NEW or RESOLVED
one at or above its installation's floor is a line, a flapping one a single
FLAPPING line (beekeeper alerts watch); only one watch at a time reads them.

Once the supervisor has served supervisor.shift, RELAY DUE is said at the
first quiet moment: no gated merge running or settling, no grant waiting
to be claimed and no claim queued. It is said once, and again only when a
quiet moment follows a busy one; never while a relay is open.

--notify also sends the events that need a person to the desktop's
notification service (org.freedesktop.Notifications on the session bus):
the kinds in notify.kinds, a note or timer falling due (due), the machine
near its OOM line (oom-line), a kernel OOM kill outside a build slot or a
systemd-oomd kill (oom-kill), the GitHub budget under the floor (budget), a
stale lease (stale-lease) and a supervisor whose session ended with no
successor (no-supervisor). Each event is one notification however many
watches notify: the first to claim it in notify.json sends it. A lasting
condition notifies again after notify.repeat (30m); notify.quietHours hold
everything but a critical one and send what they held as one notification
when they end. Nothing routine notifies. With no notification service the
watch runs on, prints its lines and says so once.

--standby is for a watch that runs when no supervisor does (the
beekeeper-notify user unit): while a supervisor's session runs it leaves
the notes, timers, session records and relays to the supervisor's watch,
and it never reads the alerts, so it takes nothing from the supervisor's
view.

Runs until killed. --once polls once and exits.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			w := &watcher{app: a, standby: standby, last: map[string]time.Time{}, seenKills: map[string]bool{}, reported: map[string]bool{}}
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
	mu         sync.Mutex
	last       map[string]time.Time
	lastPoll   time.Time
	lastBudget time.Time
	scopeOOM   int64
	sessions   map[string]*claude.Session
	seenKills  map[string]bool
	// reported are the stale leases and gone supervisors said once.
	reported map[string]bool
	// notifier sends the events that need a person (--notify); nil prints only.
	notifier *notify.Notifier
	// standby leaves a running supervisor's events to its watch.
	standby bool
	// supervisorMissed counts the polls the recorded supervisor's session
	// has not run in.
	supervisorMissed int
	// records are the session records of the last poll: their sessions'
	// ends get the record's line instead of the SESSIONS ended one.
	records []state.Record
}

func (w *watcher) run(ctx context.Context, once bool) error {
	w.lastPoll = time.Now().Add(-w.cfg.Watch.Interval.Duration)
	if p := machine.FindScope(); p != "" {
		w.scopeOOM = machine.ReadScope(p).OOMKills
	} else {
		w.emitNow("scope", "no Claude Desktop scope found; watching the machine numbers only")
	}
	var wg sync.WaitGroup
	if !once && !w.standby {
		wg.Go(func() { w.watchAlerts(ctx) })
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
			if owner.PID != other {
				other = owner.PID
				w.emitNow("alerts", "ALERTS read by the watch with pid %d; this one takes over when it ends", other)
			}
		default:
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

// emit prints a breach at most once per watch.repeat per key and returns
// the line, empty when it was folded away.
func (w *watcher) emit(key, format string, args ...any) string {
	w.mu.Lock()
	now := time.Now()
	if t, ok := w.last[key]; ok && now.Sub(t) < w.cfg.Watch.Repeat.Duration {
		w.mu.Unlock()
		return ""
	}
	w.last[key] = now
	w.mu.Unlock()
	line := fmt.Sprintf(format, args...)
	w.emitNow(key, "%s", line)
	return line
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
		if m.AvailableMiB < th.AvailMinMiB {
			w.oomLine(ctx, w.emit("avail", "LOW RAM: %d MiB available (swap used %d MiB)", m.AvailableMiB, m.SwapUsedMiB))
		}
		if m.SwapUsedMiB > th.SwapMaxMiB {
			w.oomLine(ctx, w.emit("swap", "SWAP near the oomd trigger: %d of %d MiB used (systemd-oomd kills at 90%%)", m.SwapUsedMiB, m.SwapTotalMiB))
		}
	}
	if l, err := machine.ReadLoad(); err == nil && l[0] > th.LoadMax {
		w.emit("load", "HIGH LOAD: %.0f (image imports into a fresh lab reach 35-53 on an encrypted disk)", l[0])
	}
	if psi, err := machine.ReadPSIFull60(); err == nil && psi > th.PSIMax {
		w.oomLine(ctx, w.emit("psi", "MEMORY PRESSURE: full avg60 %.0f%%", psi))
	}
	if d, err := machine.ReadDisk("/tmp"); err == nil && d.UsedMiB > th.TmpMaxMiB {
		w.emit("tmp", "TMPFS /tmp at %d MiB (RAM-backed scratch)", d.UsedMiB)
	}
	if d, err := machine.ReadDisk("/"); err == nil && d.FreeMiB < th.DiskMinMiB {
		w.emit("disk", "LOW DISK: / has %d GiB free (go clean -cache; prune unreferenced images and volumes)", d.FreeMiB/1024)
	}
	if p := machine.FindScope(); p != "" {
		s := machine.ReadScope(p)
		if s.AnonMiB > th.ScopeAnonMaxMiB {
			w.oomLine(ctx, w.emit("scopeanon", "DESKTOP SCOPE anonymous memory %d MiB (only anon cannot be reclaimed; archiving idle sessions frees it)", s.AnonMiB))
		}
		if s.CurrentMiB > th.ScopeMaxMiB {
			w.oomLine(ctx, w.emit("scope", "DESKTOP SCOPE near its hard cap: %d MiB RAM + %d MiB swap (max %s)", s.CurrentMiB, s.SwapMiB, s.Max))
		}
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
	sessions := claude.Discover(w.cfg, t, w.now)
	w.kills(ctx, since, sessions, t)
	w.pending(ctx, sessions)
	w.sessionChanges(sessions)
	w.staleLeases(ctx, sessions)

	if w.now.Sub(w.lastBudget) >= th.BudgetEvery.Duration {
		w.lastBudget = w.now
		b, err := w.probeBudget(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
		case err != nil:
			w.emit("budget-error", "GitHub budget unknown: %v", err)
		case b.Remaining < w.cfg.GitHub.Floor:
			l := w.emit("budget", "GITHUB BUDGET %d of %d, under the floor %d: hold GitHub work until the reset at %s",
				b.Remaining, b.Limit, w.cfg.GitHub.Floor, b.Reset.Local().Format("15:04"))
			w.notify(ctx, notify.Budget, "", "beekeeper: GitHub budget under the floor", l+"\nbeekeeper budget")
		}
	}
	if w.notifier != nil {
		w.notifier.Flush(ctx, w.now)
	}
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
	for _, line := range groupKills(fresh) {
		w.emitNow("kern", "KERNEL OOM: %s", line)
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
		w.emitNow("sessions", "SESSIONS ended (CLI gone: paused, closed or crashed): %s", strings.Join(ended, ", "))
	}
	if len(restarted) > 0 {
		w.emitNow("sessions", "SESSIONS restarted (a new CLI: its context may be fresh, send it its state): %s", strings.Join(restarted, ", "))
	}
	w.sessions = cur
}

// watchParty is who the watch's events are by.
var watchParty = state.Party{Name: "beekeeper watch"}

// pending prints the notes and timers that fell due, the recorded sessions
// that ended, a relay taken or expired and the relay due after the shift.
// The state keeps that they were reported, so each is one line however many
// watches run; the state is written only then.
func (w *watcher) pending(ctx context.Context, sessions []*claude.Session) {
	st, err := w.store.Read()
	if err != nil {
		w.emit("state", "cannot read the state: %v", err)
		return
	}
	w.records = st.Records
	supervised := w.supervisorGone(ctx, st, sessions)
	if w.standby && supervised {
		return // the supervisor's watch reports them
	}
	q := w.quietness(ctx, st, sessions)
	fire := func(st *state.State) ([]string, []state.Event, bool) {
		lines, evs := firePending(st, sessions, w.now)
		rl, re := fireRelay(st, w.now)
		sl, se, changed := fireShift(st, q, w.now, w.cfg.Supervisor.Shift.Duration)
		lines, evs = append(append(lines, rl...), sl...), append(append(evs, re...), se...)
		return lines, evs, changed || len(lines) > 0 || len(evs) > 0
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
		w.emit("state", "cannot write the state: %v", err)
		return
	}
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
// session has not run for two polls in a row with no relay open; it
// reports whether a supervisor runs.
func (w *watcher) supervisorGone(ctx context.Context, st *state.State, sessions []*claude.Session) bool {
	s := st.Supervisor
	if s == nil {
		w.supervisorMissed = 0
		return false
	}
	if _, live := claude.Live(sessions, s.Party); live || st.Relay.Open(w.now) {
		w.supervisorMissed = 0
		return live
	}
	w.supervisorMissed++
	key := s.Name + "@" + s.Since.UTC().Format(time.RFC3339)
	if w.supervisorMissed < 2 || w.reported["supervisor "+key] {
		return false
	}
	w.reported["supervisor "+key] = true
	l := fmt.Sprintf("SUPERVISOR GONE: %q, supervising since %s, ended with no successor (beekeeper handover --prompt starts one)", s.Name, clock(w.now, s.Since))
	w.emitNow("supervisor", "%s", l)
	w.notify(ctx, notify.NoSupervisor, key, "beekeeper: no supervisor", l)
	return false
}

// quietness reads whether the machine is at a quiet moment, once the
// supervisor's shift is over: outside the state lock, since a settling
// merge's installation is read with kubectl.
func (w *watcher) quietness(ctx context.Context, st *state.State, sessions []*claude.Session) quietness {
	if !shiftOver(st, sessions, w.now, w.cfg.Supervisor.Shift.Duration) {
		return quietness{}
	}
	holders, err := lease.Dir(w.cfg.LeaseDir).List()
	if err != nil {
		return quietness{checked: true, busy: fmt.Sprintf("the leases cannot be read: %v", err)}
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
	return quietness{checked: true, busy: busyWith(st, w.cfg, heldMap(holders), w.now, proc.Alive, rolled)}
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

func (w *watcher) staleLeases(ctx context.Context, sessions []*claude.Session) {
	holders, err := lease.Dir(w.cfg.LeaseDir).List()
	if err != nil {
		return
	}
	for _, h := range holders {
		v := w.leaseView(sessions, h)
		key := h.Env + "@" + h.Since
		if v.State == holderGone && !w.reported["lease "+key] {
			w.reported["lease "+key] = true
			l := fmt.Sprintf("STALE LEASE: %s is held by %q, whose session no longer runs (%s)", h.Env, v.Name, truncate(h.Purpose, 60))
			w.emitNow("lease", "%s", l)
			w.notify(ctx, notify.StaleLease, key, "beekeeper: stale lease "+h.Env, l+"\nbeekeeper lease status "+h.Env)
		}
	}
}
