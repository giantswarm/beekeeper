package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/takeover"
	"github.com/giantswarm/beekeeper/internal/tui"
)

// uiSlowEvery is how often the screen re-reads what is slow to ask: the
// GitHub budget and the installations' upgrades.
const uiSlowEvery = time.Minute

// uiRefreshEvery is how often the screen re-reads its fast sources: the
// live state, the sessions and the machine figures. What is slow to ask
// (the budget, the upgrades) is paced by uiSlowEvery on the collector.
const uiRefreshEvery = 2 * time.Second

// collector is the screen's tui.Source: one refresh per Data call, built
// from the same view code the commands print. It reads only: the screen
// takes no lease and lifts no hold.
type collector struct {
	a *app

	// budgetEvery and upgradesEvery pace the slow readings; 0 leaves that
	// reading off entirely (the tests run that way).
	budgetEvery, upgradesEvery time.Duration

	// mu guards the cached slow readings below. The readings run in the
	// background (busy says one is running) so a refresh never waits on
	// the network: the screen shows the last reading until a new one
	// lands.
	mu sync.Mutex
	// budgetTry is when the probe was last started, budgetAt when it last
	// succeeded with the reading itself; budgetErr is the last attempt's
	// error, "" when it succeeded.
	budgetTry, budgetAt time.Time
	budgetBusy          bool
	lastBudget          github.Budget
	budgetErr           string
	// upgradesTry is when the upgrades were last started; lastUpgrades and
	// upgradesErrs are that reading's words and error sentences.
	upgradesTry  time.Time
	upgradesBusy bool
	lastUpgrades []string
	upgradesErrs []string
}

// newCollector returns the screen's source with the slow readings on
// uiSlowEvery.
func newCollector(a *app) *collector {
	return &collector{a: a, budgetEvery: uiSlowEvery, upgradesEvery: uiSlowEvery}
}

// Data reads everything the screen shows in one go. Only the state read
// itself fails the refresh; every other failure is one sentence of Errors
// with the rest of the data intact.
func (c *collector) Data(ctx context.Context) (*tui.Data, error) {
	a := c.a
	a.now = time.Now()
	v, err := a.collect(true)
	if err != nil {
		return nil, err
	}
	st := v.st
	sv := a.supervision(st, v.raw)
	t, terr := plat.Machine.Processes()
	var errs []string
	if terr != nil {
		errs = append(errs, "process table: "+terr.Error())
	}
	var waits []wait
	if t != nil {
		waits = findWaits(t, v.raw, a.now)
	}
	d := &tui.Data{
		At:       a.now,
		Machine:  c.uiMachineView(ctx, v, t, waits, &errs),
		Sessions: c.withApprovals(uiSessions(v, waitsBy(waits))),
		Totals:   uiTotals(v.Totals),
		Overlaps: uiOverlaps(v.Overlaps),
		Holds:    uiHolds(a.activeHolds(st)),
		Lanes:    a.uiLanes(st),
		Roles:    a.uiRoles(st, v.raw, sv),
		Agents:   uiAgents(a.agentViews(st, v.raw)),
		Notes:    uiNotes(st.Notes),
		Timers:   uiTimers(st.Timers),
		Records:  uiRecords(st.Records),
		Alerts:   a.uiAlerts(&errs),
		Events:   a.uiEvents(&errs),
	}
	d.Status = tui.Status{SupervisorLive: sv.live, SupervisorGone: sv.down()}
	if st.Supervisor != nil {
		d.Status.Supervisor = st.Supervisor.Name
	}
	if s, err := a.status(); err != nil {
		errs = append(errs, "status: "+err.Error())
	} else {
		d.Status.LeaseCount, d.Status.HoldCount, d.Status.Due = len(s.Leases), len(s.Holds), s.Due
	}
	if l, err := a.leases(); err != nil {
		errs = append(errs, "leases: "+err.Error())
	} else {
		d.Leases = uiLeases(l)
	}
	d.Errors = errs
	c.uiBudgetView(st, t, v.raw, d)
	c.uiUpgradesView(st, d)
	return d, nil
}

// running discovers the running sessions on its own clock: Tail and Send
// run beside a refresh, which owns the app's.
func (c *collector) running() ([]*claude.Session, error) {
	t, err := plat.Machine.Processes()
	if err != nil {
		return nil, err
	}
	return discover(c.a.cfg, t, time.Now()), nil
}

// Tail reads the last turns of one session's transcript, as `beekeeper
// tail` does, with its tool calls among them: what it does right now.
func (c *collector) Tail(_ context.Context, session string, turns int) ([]tui.Turn, error) {
	raw, err := c.running()
	if err != nil {
		return nil, err
	}
	ts, err := sessionTail(raw, session, turns, true)
	if err != nil {
		return nil, err
	}
	out := make([]tui.Turn, 0, len(ts))
	for _, t := range ts {
		out = append(out, tui.Turn{At: t.At, Role: t.Role, Text: t.Text})
	}
	return out, nil
}

// Send delivers the person's message to one running session, stamped as
// theirs through the screen: by name to a Claude Code session's CLI, as
// SendMessage does, or into the inbox of an omp agent beekeeper started.
// It returns where the message went.
func (c *collector) Send(ctx context.Context, session, text string) (string, error) {
	a := c.a
	raw, err := c.running()
	if err != nil {
		return "", err
	}
	s, err := claude.Resolve(raw, session)
	if err != nil {
		return "", err
	}
	return a.messageSession(ctx, raw, s, text)
}

// messageSession delivers the person's text to s, one of the running
// sessions, and says where it went.
func (a *app) messageSession(ctx context.Context, sessions []*claude.Session, s *claude.Session, text string) (string, error) {
	person := a.uiPerson()
	msg := fmt.Sprintf("From %s through beekeeper ui: %s", person, strings.TrimSpace(text))
	by := state.Party{Name: person}
	if s.Harness == omp.Harness {
		id, ok := strings.CutPrefix(s.HostID, omp.HostPrefix)
		if !ok {
			return "", refused("%s: an omp session beekeeper did not start takes no message: omp has no way into an interactive session", s.Name)
		}
		if err := a.sendOmp(s.Name, id, msg); err != nil {
			return "", err
		}
		_ = a.store.Log(event(by, "ui.message", "%s: written to its omp inbox", s.Name))
		return "in its omp inbox: it runs at the next tool round or as the next turn", nil
	}
	name, err := uniqueName(sessions, s)
	if err != nil {
		return "", err
	}
	if err := a.peerSend(ctx, name, msg); err != nil {
		return "", err
	}
	_ = a.store.Log(event(by, "ui.message", "%s: sent by name to its CLI %d", s.Name, s.PID))
	return "queued in its CLI: it runs at the next tool call or as the next turn", nil
}

// uiPerson is the name the screen acts under: guide.person, else "the
// person".
func (a *app) uiPerson() string { return cmp.Or(a.cfg.Guide.Person, "the person") }

// takeoverDir is the take-over folder the hook reads.
func (c *collector) takeoverDir() string { return takeover.Dir(c.a.cfg.StateDir) }

// TakeOver brings the approvals of the Claude Code session id to this
// screen: the PermissionRequest hook holds them for its answer while the
// screen runs. An omp session has none: omp agents run in yolo mode.
func (c *collector) TakeOver(_ context.Context, id string) error {
	raw, err := c.running()
	if err != nil {
		return err
	}
	s, err := claude.Resolve(raw, id)
	if err != nil {
		return err
	}
	if s.Harness == omp.Harness {
		return refused("%s: an omp session asks for no approvals (beekeeper's omp agents run in yolo mode): nothing to take over", s.Name)
	}
	if s.ID == "" {
		return refused("%s: no session id is known, so its approvals cannot be told apart", s.Name)
	}
	person := c.a.uiPerson()
	if err := takeover.Take(c.takeoverDir(), s.ID, takeover.Flag{PID: os.Getpid(), By: person, Since: time.Now().UTC()}); err != nil {
		return err
	}
	_ = c.a.store.Log(event(state.Party{Name: person}, "ui.takeover", "%s: its approvals come to the screen", s.Name))
	return nil
}

// Release hands the approvals of session id back to its own window.
func (c *collector) Release(_ context.Context, id string) error {
	if err := takeover.Release(c.takeoverDir(), id); err != nil {
		return err
	}
	_ = c.a.store.Log(event(state.Party{Name: c.a.uiPerson()}, "ui.release", "%s: its approvals go to its own window", id))
	return nil
}

// Answer allows or denies the request the hook holds for session id.
func (c *collector) Answer(_ context.Context, id, request string, allow bool) error {
	d := takeover.Decision{Behavior: takeover.Allow}
	if !allow {
		d = takeover.Decision{Behavior: takeover.Deny, Message: "Denied by " + c.a.uiPerson() + " on beekeeper ui."}
	}
	return takeover.Answer(c.takeoverDir(), id, request, d)
}

// withApprovals marks the sessions a running screen took over and the
// requests the hook holds for each.
func (c *collector) withApprovals(ss []tui.Session) []tui.Session {
	dir := c.takeoverDir()
	for i := range ss {
		s := &ss[i]
		if s.ID == "" {
			continue
		}
		if _, ok := takeover.Taken(dir, s.ID, proc.Alive); !ok {
			continue
		}
		s.TakenOver = true
		rs, _ := takeover.Pending(dir, s.ID)
		for _, r := range rs {
			s.Approvals = append(s.Approvals, tui.Approval{ID: r.ID, At: r.At,
				Gist: claude.ToolGist(r.Tool, r.Input), Detail: inputLines(r.Input)})
		}
	}
	return ss
}

// inputLines is a tool call's input as the screen shows it: a line per
// field, its text's lines under it, the fields in name order.
func inputLines(raw json.RawMessage) []string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return nil
	}
	var out []string
	for _, k := range slices.Sorted(maps.Keys(in)) {
		v, ok := in[k].(string)
		if !ok {
			b, _ := json.Marshal(in[k])
			v = string(b)
		}
		lines := strings.Split(strings.TrimSpace(v), "\n")
		out = append(out, k+": "+lines[0])
		for _, l := range lines[1:] {
			out = append(out, "  "+l)
		}
	}
	return out
}

// uiCmd is `beekeeper ui`: the person's screen.
func (a *app) uiCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ui",
		Short: "The person's screen: every session, the machine, the shares, the roles and the log",
		Long: `Open the person's screen: every running session and what it is on, the
machine's memory, swap, pressure, build slots and kind labs, the GitHub
budget and who is drawing on it, the leases, holds and merge lanes, the
supervisor and the guide, the agents, notes and timers, the installations'
alerts and the event log. Press q to quit. Enter on a session follows it
live: its history, then the current turn with its tool calls as they land;
k and j scroll back and forward, G follows again. m there writes it a
message, stamped as the person's through the screen: a Claude Code session
gets it by name at its next tool call or as its next turn, an omp agent
beekeeper started in its inbox; an omp session the person runs in its own
terminal takes none.

t there takes a Claude Code session over: its permission requests come to
the screen, with its turns beside them, and a allows, d denies the oldest.
The PermissionRequest hook holds a taken-over session's request for the
screen up to 290 s; unanswered, or once t hands the session back or the
screen quits, the request goes to the session's own window, and the pane
says so. Everything else in that window stays as it was, and a session not
taken over is decided at once, as before. The hook entry needs a timeout
of 300 s (beekeeper install writes it).

Besides the messages it sends, it reads only: the screen takes no lease and lifts no hold — those stay with
the commands, whose exit codes the sessions gate on. It refreshes every
two seconds; the GitHub budget and the installations' upgrades are
re-read at most every minute.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			// The screen is built on the process table: without it, refuse
			// as sessions does instead of opening an empty screen.
			if _, err := plat.Machine.Processes(); platform.Missing(err) {
				return err
			}
			return tui.Run(newCollector(a), tui.Options{Interval: uiRefreshEvery})
		},
	}
	return c
}

// uiMachineView reads the guarded side: memory, pressure, the desktop
// scope, disks, build slots, kind labs, the waits and the OOM kills of the
// last hour. What cannot be read is one sentence of errs.
func (c *collector) uiMachineView(ctx context.Context, v *view, t *proc.Table, waits []wait, errs *[]string) tui.Machine {
	a := c.a
	m := tui.Machine{}
	if mem, err := machine.ReadMem(); err != nil {
		*errs = append(*errs, "memory: "+err.Error())
	} else {
		m.Mem = tui.Mem{TotalMiB: mem.TotalMiB, AvailableMiB: mem.AvailableMiB,
			SwapTotalMiB: mem.SwapTotalMiB, SwapUsedMiB: mem.SwapUsedMiB, ShmemMiB: mem.ShmemMiB}
	}
	m.Load, _ = machine.ReadLoad()
	m.PSIFull60, _ = machine.ReadPSIFull60()
	if p := machine.FindScope(); p != "" {
		if sc := machine.ReadScope(p); sc != nil {
			m.Scope = &tui.Scope{CurrentMiB: sc.CurrentMiB, AnonMiB: sc.AnonMiB, SwapMiB: sc.SwapMiB,
				High: sc.High, Max: sc.Max, SwapMax: sc.SwapMax, OOMKills: sc.OOMKills, HighEvents: sc.HighEvents}
		}
	}
	if tmp, err := machine.ReadDisk("/tmp"); err == nil {
		m.Tmp = uiDisk(tmp)
	}
	if root, err := machine.ReadDisk("/"); err == nil {
		m.Root = uiDisk(root)
	}
	for _, sl := range machine.ReadSlots(a.cfg.Memcap.SlotDir, a.cfg.Memcap.Slots) {
		m.Slots = append(m.Slots, tui.Slot{N: sl.N, Free: sl.Free, Holder: sl.Holder})
	}
	clusters, err := machine.KindClusters(ctx)
	if err != nil {
		m.ClustersErr, *errs = err.Error(), append(*errs, "kind clusters: "+err.Error())
	}
	for _, cl := range clusters {
		m.Clusters = append(m.Clusters, tui.Cluster{Name: cl.Name, Nodes: cl.Nodes, MemMiB: cl.MemMiB, RunningFor: cl.RunningFor})
	}
	for _, s := range v.raw {
		m.CLIMemMiB += s.MemMiB
	}
	for _, w := range waits {
		m.Waits = append(m.Waits, tui.Wait{PID: w.PID, Session: w.Session, Args: w.Args, Elapsed: w.Elapsed})
	}
	if t == nil {
		return m
	}
	kills, err := plat.Machine.OOMKills(ctx, a.now.Add(-time.Hour))
	if err != nil {
		*errs = append(*errs, "OOM kills: "+err.Error())
	} else {
		runs := &runIndex{store: a.store}
		for _, k := range kills {
			m.OOM = append(m.OOM, tui.OOMKill{At: k.At, PID: k.PID, Task: k.Task, AnonMiB: k.AnonMiB,
				Constraint: k.Constraint, Memcg: k.Memcg, Owner: oomOwner(k, clusters, v.raw, t, runs)})
		}
	}
	if oomd, err := plat.Machine.OomdKills(ctx, a.now.Add(-time.Hour)); err != nil {
		*errs = append(*errs, "systemd-oomd kills: "+err.Error())
	} else {
		m.Oomd = oomd
	}
	return m
}

// uiBudgetView fills the budget from the cache: the last probe reading,
// the state's last one when no probe ever succeeded, the github hold and
// who is drawing. A reading older than budgetEvery starts a background
// probe.
func (c *collector) uiBudgetView(st *state.State, t *proc.Table, sessions []*claude.Session, d *tui.Data) {
	a := c.a
	b := tui.Budget{Floor: a.cfg.GitHub.Floor}
	if hold, held := activeHold(st, a, "github"); held {
		b.Held, b.HoldReason = true, hold.Reason
	}
	if t != nil {
		b.Pollers = a.uiPollers(t, sessions)
	}
	c.mu.Lock()
	if c.budgetEvery > 0 && !c.budgetBusy && time.Now().After(c.budgetTry.Add(c.budgetEvery)) {
		c.budgetBusy, c.budgetTry = true, time.Now()
		go c.probeBudget()
	}
	var g *github.GraphQL
	if !c.budgetAt.IsZero() {
		b.At, b.Limit, b.Remaining, b.Reset = c.budgetAt, c.lastBudget.Limit, c.lastBudget.Remaining, c.lastBudget.Reset
		g = c.lastBudget.GraphQL
	} else if st.Budget != nil {
		b.At, b.Limit, b.Remaining, b.Reset = st.Budget.At, st.Budget.Limit, st.Budget.Remaining, st.Budget.Reset
		g = graphqlOf(st.Budget.GraphQL)
	}
	if g != nil {
		b.GraphQL, b.GraphQLRefused = graphqlText(a, g), g.Blocks(a.now)
	}
	b.Err = c.budgetErr
	c.mu.Unlock()
	d.Budget = b
}

// probeBudget reads the GitHub budget off its own clock, off the screen's
// refresh path, and leaves the reading in the cache.
func (c *collector) probeBudget() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := c.a.probeBudget(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.budgetBusy = false
	if err != nil {
		if ctx.Err() == nil {
			c.budgetErr = err.Error()
		}
		return
	}
	c.budgetErr, c.lastBudget, c.budgetAt = "", b, time.Now().UTC()
}

// uiUpgradesView fills the installations' upgrade lines from the cache and
// starts a background read when the cached one is older than
// upgradesEvery.
func (c *collector) uiUpgradesView(st *state.State, d *tui.Data) {
	c.mu.Lock()
	if c.upgradesEvery > 0 && !c.upgradesBusy && time.Now().After(c.upgradesTry.Add(c.upgradesEvery)) {
		c.upgradesBusy, c.upgradesTry = true, time.Now()
		go c.readUpgrades(st)
	}
	d.Upgrades = slices.Clone(c.lastUpgrades)
	d.Errors = append(d.Errors, c.upgradesErrs...)
	c.mu.Unlock()
}

// readUpgrades asks the installations about their running upgrades off the
// refresh path and leaves the words in the cache.
func (c *collector) readUpgrades(st *state.State) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a := c.a
	statuses := a.readUpgrades(ctx, st, time.Now())
	if ctx.Err() != nil {
		c.mu.Lock()
		c.upgradesBusy = false
		c.mu.Unlock()
		return
	}
	var errs []string
	for _, s := range statuses {
		if s.Err != "" {
			errs = append(errs, fmt.Sprintf("upgrades: %s unreadable: %s", s.Installation, s.Err))
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.upgradesBusy, c.lastUpgrades, c.upgradesErrs = false, upgradeWords(statuses, time.Now()), errs
}

// uiPollers are the gh and devctl processes drawing on the budget, as
// budget lists them: longest-running first.
func (a *app) uiPollers(t *proc.Table, sessions []*claude.Session) []tui.Poller {
	var out []tui.Poller
	for _, p := range githubCallers(a, t, sessions) {
		out = append(out, tui.Poller(p))
	}
	return out
}

// waitsBy summarizes each long-running command a session sits on.
func waitsBy(ws []wait) map[string]string {
	out := map[string]string{}
	for _, w := range ws {
		if w.Session == "" {
			continue
		}
		out[w.Session] = strings.TrimSpace(dur(w.Elapsed) + " " + commandName(w.Args) + " " + tailArgs(w.Args))
	}
	return out
}

// uiSessions maps the collected sessions to what the screen shows.
func uiSessions(v *view, waits map[string]string) []tui.Session {
	out := make([]tui.Session, 0, len(v.Sessions))
	for _, s := range v.Sessions {
		x := tui.Session{
			PID: s.PID, ID: s.ID, Name: s.Name, Role: s.Role, Harness: s.Harness, State: s.State, Cwd: s.Cwd, Repo: s.Repo, Branch: s.Branch,
			Model: s.Model, Permission: s.Permission, Started: s.Started, LastActive: s.LastActive,
			MemMiB: s.MemMiB, Leases: slices.Clone(s.Leases), Waits: waits[s.Name],
		}
		if s.Waiting != nil {
			x.Waiting = s.Waiting.Action
		}
		x.Work = append(slices.Clone(s.Work.Refs), s.Work.Repos...)
		if s.Serves != nil {
			x.Serves = recordText(*s.Serves)
		}
		if m := s.Metrics; m != nil {
			x.LastHour, x.Total = uiCounter(m.LastHour), uiCounter(m.Total)
			x.Idle = m.Idle
			x.Context, x.ContextWindow, x.ContextFill = m.Context, m.ContextWindow, m.ContextFill
			x.GitHubProcesses = m.GitHubProcesses
			x.Merges = tui.Merges{Queued: m.Merges.Queued, Merged: m.Merges.Merged, Refused: m.Merges.Refused, Failed: m.Merges.Failed}
			for _, sc := range m.Scopes {
				x.Scopes = append(x.Scopes, tui.RunScope{Unit: sc.Unit, Command: sc.Command, MemMiB: sc.MemMiB})
			}
		}
		for _, cmd := range s.Commands {
			x.Commands = append(x.Commands, tui.Command{PID: cmd.PID, Args: cmd.Args, Elapsed: cmd.Elapsed, Remaining: cmd.Remaining})
		}
		out = append(out, x)
	}
	return out
}

// uiDisk maps a filesystem's usage to the screen's.
func uiDisk(d machine.Disk) tui.Disk {
	return tui.Disk{Path: d.Path, UsedMiB: d.UsedMiB, FreeMiB: d.FreeMiB}
}

// uiCounter maps one span's figures to the screen's Counter.
func uiCounter(c claude.Counts) tui.Counter {
	return tui.Counter{Busy: c.Busy, Turns: c.Turns, ToolCalls: c.ToolCalls,
		ToolErrors: c.ToolErrors, GitHubCalls: c.GitHubCalls, Cost: c.CostUSD}
}

// uiTotals maps the figures over all sessions.
func uiTotals(t *metricsTotals) tui.Totals {
	if t == nil {
		return tui.Totals{}
	}
	out := tui.Totals{Total: uiCounter(t.Total), LastHour: uiCounter(t.LastHour),
		GitHubProcesses: t.GitHubProcesses,
		Merges:          tui.Merges{Queued: t.Merges.Queued, Merged: t.Merges.Merged, Refused: t.Merges.Refused, Failed: t.Merges.Failed}}
	for _, s := range t.Top {
		out.Top = append(out.Top, tui.TopSession{Name: s.Name, LastHour: uiCounter(s.LastHour), ContextFill: s.ContextFill})
	}
	return out
}

// uiOverlaps maps what more than one session is on.
func uiOverlaps(os []claude.Overlap) []tui.Overlap {
	out := make([]tui.Overlap, 0, len(os))
	for _, o := range os {
		out = append(out, tui.Overlap{Kind: o.Kind, Key: o.Key, Sessions: slices.Clone(o.Sessions)})
	}
	return out
}

// uiLeases maps what is held, what is free and the grant queues.
func uiLeases(l *leaseList) tui.Leases {
	out := tui.Leases{Queues: map[string][]tui.Grant{}}
	for _, h := range l.Held {
		out.Held = append(out.Held, tui.Lease{Resource: h.Env, Holder: h.Holder.Holder, Name: h.Name,
			Purpose: h.Purpose, State: h.State, Since: h.SinceTime(), UpgradeUnblock: h.UpgradeUnblock})
	}
	out.Free = slices.Clone(l.Free)
	for res, q := range l.Queues {
		for _, g := range q {
			out.Queues[res] = append(out.Queues[res], tui.Grant{Resource: g.Resource, To: g.To.Name, By: g.By.Name, At: g.At})
		}
	}
	return out
}

// uiHolds maps the active holds.
func uiHolds(hs []state.Hold) []tui.Hold {
	out := make([]tui.Hold, 0, len(hs))
	for _, h := range hs {
		out = append(out, tui.Hold{Target: h.Target, Reason: h.Reason, By: h.By.Name, At: h.At, Until: h.Until,
			Except: h.Except, Tool: h.Tool, ToolFrom: h.ToolFrom, ToolRelease: h.ToolRelease})
	}
	return out
}

// uiLanes maps every lane's queue, its hold and its stall.
func (a *app) uiLanes(st *state.State) []tui.Lane {
	vs := a.laneViews(st)
	out := make([]tui.Lane, 0, len(vs))
	for _, v := range vs {
		l := tui.Lane{Name: v.Name, Installation: v.Installation}
		if m := v.Running; m != nil {
			merged := uiMerge(*m)
			l.Running = &merged
		}
		for _, m := range v.AllSettling {
			merged := uiMerge(*m)
			l.Settling = append(l.Settling, &merged)
		}
		for _, m := range v.Waiting {
			l.Waiting = append(l.Waiting, uiMerge(m))
		}
		if v.Hold != nil {
			l.Hold = fmt.Sprintf("held by %q until %s: %s", v.Hold.By.Name, untilText(a, *v.Hold), v.Hold.Reason)
		}
		if v.Stall != nil {
			l.Stall = a.stallText(*v.Stall)
		}
		out = append(out, l)
	}
	return out
}

// uiMerge maps one gated merge.
func uiMerge(m state.Merge) tui.Merge {
	return tui.Merge{Key: m.Key(), By: m.By.Name, Phase: m.Phase, Joined: m.Joined, Started: m.Started,
		Finished: m.Finished, Exit: m.Exit, Seeded: m.Seeded, Outside: m.Outside,
		Retrying: m.Retrying(), Release: m.Release, Roll: slices.Clone(m.Roll)}
}

// uiRoles is the supervisor's and the guide's record as the screen reads
// them, each with its open relay.
func (a *app) uiRoles(st *state.State, sessions []*claude.Session, sv supervision) []tui.Role {
	out := []tui.Role{a.uiRole(supervisorRole, st.SupervisorRole(), sessions, sv)}
	guide := readHolder(st.GuideRole(), sessions, a.now, a.cfg.Guide.RestartGrace.Duration)
	return append(out, a.uiRole(guideRole, st.GuideRole(), sessions, guide))
}

// uiRole is rl's record with r and sv as the screen reads it.
func (a *app) uiRole(rl role, r state.Role, sessions []*claude.Session, sv supervision) tui.Role {
	v := tui.Role{Name: rl.name}
	rv := a.viewRole(rl, r, sessions, sv)
	if rv == nil {
		return v
	}
	v.Holder, v.Live, v.Since = rv.Name, rv.Live, rv.Since
	v.Gone, v.RestartUntil = rv.CLIGone, rv.RestartUntil
	v.Context, v.RelayAt = rv.Context, rv.RelayAt
	if r.Relay.Open(a.now) {
		v.Relay = fmt.Sprintf("to %s until %s", r.Relay.To.Name, clock(a.now, r.Relay.Expires))
	}
	return v
}

// uiAgents maps the registered agents.
func uiAgents(vs []agentView) []tui.Agent {
	out := make([]tui.Agent, 0, len(vs))
	for _, v := range vs {
		out = append(out, tui.Agent{Name: v.Name, Task: v.Task, AssignedAt: v.AssignedAt,
			IdleSince: v.IdleSince, Reachable: v.Reachable})
	}
	return out
}

// uiNotes maps the open items.
func uiNotes(ns []state.Note) []tui.Note {
	out := make([]tui.Note, 0, len(ns))
	for _, n := range ns {
		out = append(out, tui.Note{ID: n.ID, For: n.For, Text: n.Text, Default: n.Default, Due: n.Due, By: n.By.Name})
	}
	return out
}

// uiTimers maps the timers.
func uiTimers(ts []state.Timer) []tui.Timer {
	out := make([]tui.Timer, 0, len(ts))
	for _, t := range ts {
		out = append(out, tui.Timer{ID: t.ID, Due: t.Due, What: t.What, By: t.By.Name, Fired: !t.Fired.IsZero()})
	}
	return out
}

// uiRecords maps the session records.
func uiRecords(rs []state.Record) []tui.Record {
	out := make([]tui.Record, 0, len(rs))
	for _, r := range rs {
		out = append(out, tui.Record{Session: r.Session.Name, Issue: r.Issue, Waits: r.Waits})
	}
	return out
}

// uiAlerts is the alerts baseline as it stands: the running watch owns it,
// the screen only reads the file. A baseline that does not read is one
// sentence of errs.
func (a *app) uiAlerts(errs *[]string) []tui.AlertsFor {
	b, err := alerts.NewStore(a.cfg.StateDir).Load()
	if err != nil {
		*errs = append(*errs, "alerts baseline: "+err.Error())
		return nil
	}
	out := make([]tui.AlertsFor, 0, len(b.Installations))
	for _, name := range slices.Sorted(maps.Keys(b.Installations)) {
		in := b.Installations[name]
		af := tui.AlertsFor{Installation: name, Reachable: in.Reachable}
		for _, al := range in.Alerts {
			af.Alerts = append(af.Alerts, tui.Alert{Severity: al.Severity, Team: al.Team,
				Alertname: al.Alertname, Cluster: al.Cluster, Where: al.Where, Since: al.Since})
		}
		slices.SortStableFunc(af.Alerts, func(x, y tui.Alert) int {
			if c := strings.Compare(x.Alertname, y.Alertname); c != 0 {
				return c
			}
			return strings.Compare(x.Where, y.Where)
		})
		out = append(out, af)
	}
	return out
}

// uiEvents is the last two hundred lines of the event log, newest last.
func (a *app) uiEvents(errs *[]string) []tui.Event {
	evs, err := a.store.Events(200, nil)
	if err != nil {
		*errs = append(*errs, "event log: "+err.Error())
		return nil
	}
	out := make([]tui.Event, 0, len(evs))
	for _, e := range evs {
		out = append(out, tui.Event{At: e.At, Verb: e.Verb, By: e.By.Name, Detail: e.Detail})
	}
	return out
}
