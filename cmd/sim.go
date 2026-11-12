package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/sim"
	"github.com/giantswarm/beekeeper/internal/state"
)

// simForecastFile is the side file sim forecast saves for sim check.
const (
	simForecastFile = "sim-forecast.json"
	simCheckVerb    = "check"
)

// simFlags are the model's parameters a run may override.
type simFlags struct {
	workers, headroom, lanes, backlog int
	uptime, red, changes, dropped     float64
	arrivals                          float64
	days                              int
	seed                              int64
	updateBranch                      bool
	stages                            []string
}

func (f *simFlags) bind(c *cobra.Command) {
	fl := c.Flags()
	fl.IntVar(&f.workers, "workers", 0, "busy agents the loop may run at once (default capacity.ceiling)")
	fl.IntVar(&f.headroom, "review-headroom", -1, "extra slots for reviewers (default capacity.reviewHeadroom)")
	fl.IntVar(&f.lanes, "lanes", 0, "repositories merging in parallel, one merge at a time each (default 3)")
	fl.IntVar(&f.backlog, "backlog", -1, "issues on the board at the start; -1: the board never runs out")
	fl.Float64Var(&f.arrivals, "arrivals", 0, "issues filed per day on top of the backlog")
	fl.Float64Var(&f.uptime, "uptime", 1, "share of the time the loop runs, 0 to 1")
	fl.Float64Var(&f.red, "red-ci", -1, "chance a CI run is red (default 0.2)")
	fl.Float64Var(&f.changes, "changes-requested", -1, "chance a review asks for changes (default 0.2)")
	fl.Float64Var(&f.dropped, "dropped", -1, "chance a started issue ends without a pull request (default 0)")
	fl.IntVar(&f.days, "days", 7, "simulated days")
	fl.Int64Var(&f.seed, "seed", 1, "random seed")
	fl.BoolVar(&f.updateBranch, "update-branch", false, "every merge re-runs CI, as devctl pr merge --update-branch does")
	fl.StringArrayVar(&f.stages, "stage", nil, "a stage's median service time, name=duration (implementing, ci, review-queued, reviewing, fixing, merging, releasing, decision); repeatable")
}

func (f *simFlags) apply(m sim.Model) (sim.Model, error) {
	if f.workers > 0 {
		m.Workers = f.workers
	}
	if f.headroom >= 0 {
		m.ReviewHeadroom = f.headroom
	}
	if f.lanes > 0 {
		m.Lanes = f.lanes
	}
	m.Backlog = f.backlog
	m.ArrivalsPerDay = f.arrivals
	if f.uptime <= 0 || f.uptime > 1 {
		return m, usageErr("--uptime must be above 0 and at most 1")
	}
	m.LoopUptime = f.uptime
	if f.red >= 0 {
		m.RedCI = f.red
	}
	if f.changes >= 0 {
		m.ChangesRequested = f.changes
	}
	if f.dropped >= 0 {
		m.Dropped = f.dropped
	}
	if f.days <= 0 {
		return m, usageErr("--days must be positive")
	}
	m.Horizon = time.Duration(f.days) * 24 * time.Hour
	m.Seed = f.seed
	m.UpdateBranch = f.updateBranch
	for _, s := range f.stages {
		name, value, ok := strings.Cut(s, "=")
		if _, known := m.Stages[name]; !ok || !known {
			return m, usageErr("--stage %q: want <stage>=<duration>, a stage of %s", s, strings.Join(append(sim.Stages, sim.StageDecision), ", "))
		}
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return m, usageErr("--stage %q: %q is no duration", s, value)
		}
		m = m.With(name, d)
	}
	return m, nil
}

// simModel is the intended flow with this desk's capacities.
func (a *app) simModel() sim.Model {
	m := sim.Default()
	if a.cfg.Capacity.Ceiling > 0 {
		m.Workers = a.cfg.Capacity.Ceiling
	}
	m.ReviewHeadroom = a.cfg.Capacity.ReviewHeadroom
	return m
}

func (a *app) simCmd() *cobra.Command {
	var since, until string
	var flags simFlags
	window := func(c *cobra.Command) {
		c.Flags().StringVar(&since, "since", "7d", "the window's start: a duration before --until (24h, 7d), a time (15:04), a date (2006-01-02) or RFC 3339")
		c.Flags().StringVar(&until, "until", "", "the window's end (default now)")
	}
	observe := func() (sim.Observed, error) {
		to := a.timeNow()
		if until != "" {
			t, err := reportTime(a.now, until, time.Local)
			if err != nil {
				return sim.Observed{}, err
			}
			to = t
		}
		from, err := reportTime(to, since, time.Local)
		if err != nil {
			return sim.Observed{}, err
		}
		if !from.Before(to) {
			return sim.Observed{}, usageErr("--since %s is not before --until %s", from.Format(time.RFC3339), to.Format(time.RFC3339))
		}
		events, err := a.store.Events(0, func(e state.Event) bool { return sim.Verbs(e.Verb) })
		if err != nil {
			return sim.Observed{}, err
		}
		return sim.Observe(events, from, to), nil
	}
	c := &cobra.Command{
		Use:   "sim",
		Short: "Simulate how an issue should flow from its start to its release, measure how it flowed, and rank what would gain the most",
		Long: `The flow of an issue as the supervisor loop is meant to run it: a worker
starts when a slot is free and pushes a pull request; CI runs; once it is
green the loop starts a reviewer, which posts findings for the implementing worker; the lane
merges one pull request at a time; the release rolls out. The loop drives
every step but the worker's and the reviewer's turns, so while it is down
nothing starts, is reviewed or merges.

model prints that flow with its parameters: the slots (capacity.ceiling
and capacity.reviewHeadroom), the lanes, the service time of each stage
and the chances of red CI, a change request and a drop. run simulates it
(--workers, --lanes, --uptime, --stage name=duration, ... change one
parameter at a time) and prints the released issues per day, the cycle
time and each stage's wait and service. observe measures the same figures
from the event log of a window: each delivery step's residence
(delivery.step), the loop's silences (every supervisor.* event), the
busy agents against the ceiling (the USAGE lines), the starts, the
failures by step and reason, the merges' exit codes, what the workers
parked on, and the releases rolled back. compare, the default, puts both
side by side: per stage the model against the log with the causes the log
names, then the model fitted to the log (its stage times, uptime, drop
share and start rate) run with one change at a time back towards the
model, ranked by the released issues per day each change would gain.
The ranking is the order to fix things in.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			o, err := observe()
			if err != nil {
				return err
			}
			m, err := flags.apply(a.simModel())
			if err != nil {
				return err
			}
			cmp := sim.Compare(m, o)
			if a.json {
				return a.printJSON(cmp)
			}
			return a.printComparison(cmp)
		},
	}
	window(c)
	flags.bind(c)
	model := &cobra.Command{
		Use:   "model",
		Short: "Print the intended flow and its parameters",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			m, err := flags.apply(a.simModel())
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(m)
			}
			return a.printModel(m)
		},
	}
	flags.bind(model)
	run := &cobra.Command{
		Use:   "run",
		Short: "Simulate the flow with the given parameters",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			m, err := flags.apply(a.simModel())
			if err != nil {
				return err
			}
			r := sim.Run(m)
			if a.json {
				return a.printJSON(struct {
					Model  sim.Model  `json:"model"`
					Result sim.Result `json:"result"`
				}{m, r})
			}
			return a.printRun(r)
		},
	}
	flags.bind(run)
	obs := &cobra.Command{
		Use:   "observe",
		Short: "Measure the real flow from the event log",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			o, err := observe()
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(o)
			}
			return a.printObserved(o)
		},
	}
	window(obs)
	compare := &cobra.Command{
		Use:   "compare",
		Short: "The model against the log, and the changes ranked by their gain",
		Args:  cobra.NoArgs,
		RunE:  c.RunE,
	}
	window(compare)
	flags.bind(compare)
	c.AddCommand(model, run, obs, compare, a.simForecastCmd(&flags), a.simCheckCmd())
	return c
}

func (a *app) simForecastCmd(flags *simFlags) *cobra.Command {
	var horizon, fromLog string
	var noBoard bool
	save := true
	c := &cobra.Command{
		Use:   "forecast [--horizon 24h] [--from-log 7d] [--no-board] [--save]",
		Short: "Forecast the flow from the live state: what releases when, what starts, what will not move",
		Long: `Reads the desk as it is (the deliveries with their agents' states, the
roster, the timers, holds, lanes, failed merges and review requests, the
capacity verdict and board next, each the implementation's own) and runs
it forward through the model over --horizon: for every item in flight the
steps it enters and when it is released, for the free board items when a
slot takes them, and the totals by 6h, 12h and 24h. Before the run the
snapshot is read for PROBLEMS: stop is what will not move at all (the loop
down, a stop, no room while free items wait, a review recorded as requested
while no reviewer runs, a parked worker on a timer that is gone), slow what
moves late (a failed merge in backoff, items long in a step, idle agents
beside free items). --from-log fits the stage times to the log's medians of
that window; --no-board skips the GitHub read. The forecast is saved for
sim check unless --save=false. Nothing else is written.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := time.ParseDuration(horizon)
			if err != nil || h <= 0 {
				return usageErr("--horizon %q: want a duration such as 24h", horizon)
			}
			m, err := flags.apply(a.simModel())
			if err != nil {
				return err
			}
			if fromLog != "" {
				to := a.timeNow()
				from, err := reportTime(to, fromLog, time.Local)
				if err != nil {
					return err
				}
				events, err := a.store.Events(0, func(e state.Event) bool { return sim.Verbs(e.Verb) })
				if err != nil {
					return err
				}
				m = sim.FitStages(m, sim.Observe(events, from, to))
			}
			snap, err := a.simSnapshot(cmd.Context(), cmd.ErrOrStderr(), !noBoard)
			if err != nil {
				return err
			}
			f := sim.Forecasting(m, snap, h)
			if save {
				if err := a.store.WriteFile(simForecastFile, f); err != nil {
					return err
				}
			}
			if a.json {
				return a.printJSON(f)
			}
			return a.printForecast(f)
		},
	}
	c.Flags().StringVar(&horizon, "horizon", "24h", "how far to forecast")
	c.Flags().StringVar(&fromLog, "from-log", "", "fit the stage times to the log's medians of this window before now (7d, 24h)")
	c.Flags().BoolVar(&noBoard, "no-board", false, "forecast the items in flight alone, without reading the board")
	c.Flags().BoolVar(&save, "save", true, "save the forecast for sim check")
	flags.bind(c)
	return c
}

func (a *app) simCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   simCheckVerb,
		Short: "Compare the saved forecast with the desk now",
		Long: `Reads the forecast sim forecast saved and the deliveries now, and says
for every forecast item where the forecast had it and where it is: on
track, ahead, behind (by how long, with its agent and what it waits on),
stuck as forecast, or gone; then the starts and releases the forecast
expected by now against the real ones, and which of the forecast's
problems still stand.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			var f sim.Forecast
			found, err := a.store.ReadFile(simForecastFile, &f)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("no forecast saved: run beekeeper sim forecast first")
			}
			st, err := a.readSnapshot()
			if err != nil {
				return err
			}
			now := a.timeNow()
			items, _, _ := a.simItems(st)
			var released, started []string
			for _, d := range st.Deliveries {
				if d.Step == state.DeliveryDone && !d.Since.Before(f.At) {
					released = append(released, d.Ref)
				}
				if !d.Started.Before(f.At) && !d.Started.After(now) {
					started = append(started, d.Ref)
				}
			}
			c := sim.Check(f, now, items, released, started)
			if a.json {
				return a.printJSON(c)
			}
			return a.printChecked(c)
		},
	}
}

// simItems is the active deliveries with their agents' states, and the
// roster's count and reviewers.
func (a *app) simItems(st *state.State) ([]sim.Item, AgentCount, []string) {
	sessions, _, err := a.sessions()
	if err != nil && !platform.Missing(err) {
		sessions = nil
	}
	c := countAgents(st, sessions, a.now, a.cfg)
	states := map[string]string{}
	var reviewers []string
	for _, lists := range []struct {
		names []string
		state string
	}{{c.Busy, sim.AgentBusy}, {c.Waiting, sim.AgentBusy}, {c.Parked, sim.AgentParked}, {c.Idle, sim.AgentIdle}, {c.Gone, ""}} {
		for _, n := range lists.names {
			name, _, _ := strings.Cut(n, " (")
			states[name] = lists.state
			if lists.state == sim.AgentBusy && strings.HasPrefix(name, "Review ") {
				reviewers = append(reviewers, name)
			}
		}
	}
	var items []sim.Item
	for _, d := range st.Deliveries {
		if !d.Active() || d.Step == state.DeliveryUnknown {
			continue
		}
		items = append(items, sim.Item{Ref: d.Ref, Pulls: d.Pulls, Step: d.Step, Since: d.Since, Agent: d.Agent, AgentState: states[d.Agent], Waits: d.Waits})
	}
	return items, c, reviewers
}

// simSnapshot reads the desk for a forecast: the state, the roster and
// the implementation's own verdicts, capacity and board next, run in
// process with their JSON output.
func (a *app) simSnapshot(ctx context.Context, stderr io.Writer, withBoard bool) (sim.Snapshot, error) {
	st, err := a.readSnapshot()
	if err != nil {
		return sim.Snapshot{}, err
	}
	s := sim.Snapshot{At: a.timeNow(), LoopRunning: a.loopPID() != 0}
	var c AgentCount
	s.Items, c, s.Reviewers = a.simItems(st)
	s.Busy, s.Parked, s.Idle = len(c.Busy), len(c.Parked), len(c.Idle)
	for _, ag := range st.Agents {
		if ag.Park != nil {
			s.ParkedAgents = append(s.ParkedAgents, sim.ParkedAgent{Name: ag.Name, On: ag.Park.On, Waits: ag.Park.Waits})
		}
	}
	for _, t := range st.Timers {
		s.Timers = append(s.Timers, t.ID)
	}
	for _, h := range st.Holds {
		line := h.Target + ": " + h.Reason
		if !h.Until.IsZero() {
			line += " (until " + clock(a.now, h.Until) + ")"
		}
		s.Holds = append(s.Holds, line)
	}
	for _, m := range st.Merges {
		since := m.Started
		if since.IsZero() {
			since = m.Joined
		}
		s.Lanes = append(s.Lanes, sim.LaneState{Lane: m.Lane, Ref: fmt.Sprintf("%s#%d", m.Repo, m.PR), Phase: m.Phase, Since: since})
	}
	for _, f := range st.Failed {
		s.FailedMerges = append(s.FailedMerges, sim.FailedMerge{Ref: fmt.Sprintf("%s#%d", f.Repo, f.PR), At: f.At, Exit: f.Exit, Reason: f.Reason})
	}
	s.ReviewRequests = st.ReviewRequests
	s.UnmergedLimit, _ = a.unmergedPolicy()
	var capacity struct {
		Room      int      `json:"room"`
		Blocks    []string `json:"blocks"`
		Stopped   string   `json:"stopped"`
		Providers map[string]struct {
			Busy    []string `json:"busy"`
			Ceiling int      `json:"ceiling"`
			Room    int      `json:"room"`
		} `json:"providers"`
	}
	if err := a.simRunJSON(ctx, func(child *app) *cobra.Command { return child.capacityCmd() }, nil, &capacity); err != nil {
		return sim.Snapshot{}, fmt.Errorf("capacity: %w", err)
	}
	s.Room, s.Blocks, s.Stopped = capacity.Room, capacity.Blocks, capacity.Stopped
	for _, block := range s.Blocks {
		if !strings.Contains(block, "busy") || !strings.Contains(block, "ceiling") {
			s.StartBlocked = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(capacity.Providers)) {
		p := capacity.Providers[name]
		s.Providers = append(s.Providers, sim.ProviderRoom{Name: name, Busy: len(p.Busy), Ceiling: p.Ceiling, Room: p.Room})
	}
	a.simMergeBlocks(st, &s)
	if !withBoard {
		return s, nil
	}
	var next struct {
		Pick      *board.Candidate  `json:"pick"`
		Skipped   []board.Candidate `json:"skipped"`
		AfterPick []board.Candidate `json:"after_pick"`
	}
	if err := a.simRunJSON(ctx, func(child *app) *cobra.Command { return child.boardNextCmd() }, nil, &next); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: the board could not be read, the forecast is of the items in flight alone: %v\n", err)
		return s, nil
	}
	s.BoardRead = true
	add := func(c board.Candidate) {
		s.Board = append(s.Board, sim.BoardItem{Ref: c.Ref, Status: c.Status, Skip: c.Skip})
	}
	if next.Pick != nil {
		add(*next.Pick)
	}
	for _, c := range next.AfterPick {
		add(c)
	}
	for _, c := range next.Skipped {
		add(c)
	}
	a.simMergeBlocks(st, &s)
	return s, nil
}

// simMergeBlocks uses the gate's hold verdict, including lane and PR exceptions.
func (a *app) simMergeBlocks(st *state.State, s *sim.Snapshot) {
	if s.MergeBlocks == nil {
		s.MergeBlocks = map[string]string{}
	}
	check := func(ref string, pulls []string) {
		if len(pulls) == 0 {
			repo, _, _ := strings.Cut(ref, "#")
			if h, held := merge.Blocking(st, s.At, repo, -1, a.cfg.LaneOf(repo)); held {
				s.MergeBlocks[ref] = "merge held: " + h.Target + ": " + h.Reason
			}
			return
		}
		for _, pull := range pulls {
			repo, pr, ok := splitPR(pull)
			if !ok {
				continue
			}
			if h, held := merge.Blocking(st, s.At, repo, pr, a.cfg.LaneOf(repo)); held {
				s.MergeBlocks[ref] = "merge held: " + h.Target + ": " + h.Reason
			}
		}
	}
	for _, it := range s.Items {
		if it.Step != sim.StageReleasing {
			check(it.Ref, it.Pulls)
		}
	}
	for _, it := range s.Board {
		// A board issue has no PR yet; do not treat its issue number as a PR exception.
		repo, _, _ := strings.Cut(it.Ref, "#")
		if h, held := merge.Blocking(st, s.At, repo, -1, a.cfg.LaneOf(repo)); held {
			s.MergeBlocks[it.Ref] = "merge held: " + h.Target + ": " + h.Reason
		}
	}
}

// simRunJSON runs one of this app's commands in process with --json and
// decodes its output into v: the verdict is the implementation's own.
func (a *app) simRunJSON(ctx context.Context, cmd func(*app) *cobra.Command, args []string, v any) error {
	var buf bytes.Buffer
	child := *a
	child.out = &buf
	child.json = true
	c := cmd(&child)
	if args == nil {
		// nil would hand cobra this process's own arguments.
		args = []string{}
	}
	c.SetArgs(args)
	c.SetContext(ctx)
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err != nil {
		return err
	}
	return json.Unmarshal(buf.Bytes(), v)
}

func (a *app) printForecast(f sim.Forecast) error {
	s := f.Snapshot
	loop := "loop running"
	if !s.LoopRunning {
		loop = "loop NOT running"
	}
	if s.Stopped != "" {
		loop += ", stopped: " + s.Stopped
	}
	boardLine := "board not read"
	if s.BoardRead {
		free := len(s.Free())
		boardLine = fmt.Sprintf("board: %d free items, %d held or in delivery", free, len(s.Board)-free)
	}
	_, _ = fmt.Fprintf(a.out, "Forecast from %s over %s: %s, %d busy of %d slots (room %d), %d parked, %d idle; %s\n", clock(a.now, f.At), dur(f.Horizon), loop, s.Busy, s.Busy+s.Room, s.Room, s.Parked, s.Idle, boardLine)
	if len(f.Problems) > 0 {
		_, _ = fmt.Fprintf(a.out, "PROBLEMS (what will not move)\n")
		for _, p := range f.Problems {
			refs := ""
			if len(p.Refs) > 0 {
				refs = "  " + strings.Join(p.Refs, ", ")
			}
			_, _ = fmt.Fprintf(a.out, "  %-4s  %s%s\n", p.Severity, p.What, refs)
		}
	}
	w := a.table()
	_, _ = fmt.Fprintf(w, "EXPECTED\n")
	for _, t := range f.Totals {
		_, _ = fmt.Fprintf(w, "  by +%s\t%d released, %d started\n", dur(t.After), t.Released, t.Started)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	items := slices.Clone(f.Items)
	items = slices.DeleteFunc(items, func(it sim.ItemForecast) bool { return it.Step == sim.StepBoard })
	slices.SortStableFunc(items, forecastOrder)
	w = a.table()
	_, _ = fmt.Fprintf(w, "ITEMS\tNOW\tNEXT\tRELEASE\n")
	for _, it := range items {
		now := it.Step
		if !it.Since.IsZero() {
			now += " " + dur(f.At.Sub(it.Since).Truncate(time.Minute))
		}
		switch {
		case it.Stuck != "":
			_, _ = fmt.Fprintf(w, "%s\t%s\tstuck: %s\t\n", it.Ref, now, truncate(it.Stuck, 80))
		case len(it.Timeline) == 0:
			_, _ = fmt.Fprintf(w, "%s\t%s\t-\tpast the horizon\n", it.Ref, now)
		default:
			next := it.Timeline[0]
			release := "past the horizon"
			if !it.ReleaseAt.IsZero() {
				release = clock(a.now, it.ReleaseAt)
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s %s\t%s\n", it.Ref, now, next.Step, clock(a.now, next.At), release)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(f.Starts) > 0 {
		w = a.table()
		_, _ = fmt.Fprintf(w, "STARTS (board items the slots take)\n")
		for _, st := range f.Starts {
			_, _ = fmt.Fprintf(w, "  %s\t%s\n", st.Ref, clock(a.now, st.At))
		}
		return w.Flush()
	}
	return nil
}

// forecastOrder puts the stuck items first, by step, then the rest by
// their release.
func forecastOrder(a, b sim.ItemForecast) int {
	if (a.Stuck != "") != (b.Stuck != "") {
		if a.Stuck != "" {
			return -1
		}
		return 1
	}
	if a.Stuck != "" {
		return slices.Index(sim.Stages, a.Step) - slices.Index(sim.Stages, b.Step)
	}
	switch {
	case a.ReleaseAt.IsZero() && b.ReleaseAt.IsZero():
		return 0
	case a.ReleaseAt.IsZero():
		return 1
	case b.ReleaseAt.IsZero():
		return -1
	}
	return a.ReleaseAt.Compare(b.ReleaseAt)
}

func (a *app) printChecked(c sim.Checked) error {
	_, _ = fmt.Fprintf(a.out, "The forecast from %s against the desk at %s (%s later)\n", clock(a.now, c.ForecastAt), clock(a.now, c.At), dur(c.At.Sub(c.ForecastAt)))
	w := a.table()
	for _, it := range c.Items {
		if it.Verdict != sim.VerdictBehind {
			continue
		}
		agent := "no agent"
		if it.Agent != "" {
			agent = it.Agent
			if it.AgentState != "" {
				agent += " (" + it.AgentState + ")"
			}
		}
		if it.Waits != "" {
			agent += ": " + truncate(it.Waits, 80)
		}
		_, _ = fmt.Fprintf(w, "  %s\tbehind %s\tforecast %s, still %s\t%s\n", it.Ref, dur(it.Behind), it.Expected, it.Actual, agent)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(a.out, "by now: forecast %d started, %d released; real %d started, %d released\n", c.Forecast.Started, c.Forecast.Released, c.Actual.Started, c.Actual.Released)
	_, _ = fmt.Fprintf(a.out, "%s\n", c.Summary())
	w = a.table()
	for _, it := range c.Items {
		if it.Verdict != sim.VerdictAhead && it.Verdict != sim.VerdictGone {
			continue
		}
		_, _ = fmt.Fprintf(w, "  %s\t%s\tforecast %s, now %s\n", it.Ref, it.Verdict, it.Expected, it.Actual)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, outcome := range []string{sim.ProblemStanding, sim.ProblemUnchecked, sim.ProblemResolved} {
		first := true
		for _, p := range c.Problems {
			if p.Outcome != outcome {
				continue
			}
			if first {
				_, _ = fmt.Fprintf(a.out, "PROBLEMS %s\n", strings.ToUpper(outcome))
				first = false
			}
			refs := ""
			if len(p.Still) > 0 {
				refs = "  still: " + strings.Join(p.Still, ", ")
			}
			if len(p.Moved) > 0 {
				refs += "  moved: " + strings.Join(p.Moved, ", ")
			}
			_, _ = fmt.Fprintf(a.out, "  %-4s  %s%s\n", p.Severity, p.What, refs)
		}
	}
	return nil
}

func (a *app) printModel(m sim.Model) error {
	w := a.table()
	_, _ = fmt.Fprintf(w, "The flow: board -> worker (implementing) -> CI -> reviewer (review-queued, reviewing) -> fixes (fixing) -> lane (merging) -> release (releasing) -> done\n")
	_, _ = fmt.Fprintf(w, "slots\t%d busy agents, reviewers up to %d more\n", m.Workers, m.ReviewHeadroom)
	_, _ = fmt.Fprintf(w, "lanes\t%d, one merge at a time each\n", m.Lanes)
	_, _ = fmt.Fprintf(w, "loop\tup %.0f%% of the time, an outage %s on average\n", 100*m.LoopUptime, dur(m.LoopOutage))
	_, _ = fmt.Fprintf(w, "chances\tred CI %.0f%%, changes requested %.0f%%, dropped %.0f%%, a decision %.0f%%\n", 100*m.RedCI, 100*m.ChangesRequested, 100*m.Dropped, 100*m.DecisionShare)
	if m.UpdateBranch {
		_, _ = fmt.Fprintf(w, "merge\tevery merge re-runs CI (--update-branch)\n")
	}
	_, _ = fmt.Fprintf(w, "board\t%s, %.1f issues filed a day\n", backlogWord(m.Backlog), m.ArrivalsPerDay)
	_, _ = fmt.Fprintf(w, "horizon\t%s, seed %d\n", dur(m.Horizon), m.Seed)
	_, _ = fmt.Fprintf(w, "\nSTAGE\tMEDIAN\tSPREAD\tWHAT\n")
	what := map[string]string{
		sim.StageImplementing: "the worker's turn until it pushes and parks",
		sim.StageCI:           "a CI run on the head",
		sim.StageReviewQueued: "green CI until the reviewer runs (the loop's pass and the start)",
		sim.StageReviewing:    "the reviewer's turn, reporting findings to the worker",
		sim.StageFixing:       "a fixer's turn after red CI or a change request",
		sim.StageMerging:      "the gate's run: devctl pr merge with the merge-result check",
		sim.StageReleasing:    "the release workflow and the rollout",
		sim.StageDecision:     "a wait for the person's answer",
	}
	for _, name := range append(sim.Stages, sim.StageDecision) {
		s := m.Stages[name]
		_, _ = fmt.Fprintf(w, "%s\t%s\t%.1f\t%s\n", name, dur(s.Median), s.Sigma, what[name])
	}
	return w.Flush()
}

func backlogWord(n int) string {
	if n < 0 {
		return "a board that never runs out"
	}
	return fmt.Sprintf("%d issues on the board", n)
}

func (a *app) printRun(r sim.Result) error {
	w := a.table()
	_, _ = fmt.Fprintf(w, "%d started, %d released, %d dropped, %d open after %s: %.1f started and %.1f released a day\n", r.Started, r.Released, r.Dropped, r.Open, dur(r.Horizon), r.StartedPerDay, r.ReleasedPerDay)
	_, _ = fmt.Fprintf(w, "cycle start to release: median %s, p90 %s; board wait median %s\n", dur(r.Cycle.Median), dur(r.Cycle.P90), dur(r.BacklogWait.Median))
	_, _ = fmt.Fprintf(w, "slots busy %.0f%%, lanes busy %.0f%%, loop down %s\n", 100*r.WorkerUtilization, 100*r.LaneUtilization, dur(r.LoopDown))
	_, _ = fmt.Fprintf(w, "\nSTAGE\tN\tWAIT\tSERVICE\tRESIDENCE\tP90\n")
	for _, s := range r.Stages {
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\n", s.Stage, s.Residence.Count, dur(s.Wait.Median), dur(s.Service.Median), dur(s.Residence.Median), dur(s.Residence.P90))
	}
	return w.Flush()
}

func (a *app) printObserved(o sim.Observed) error {
	w := a.table()
	for _, outcome := range slices.Sorted(maps.Keys(o.Outcomes)) {
		_, _ = fmt.Fprintf(w, "outcome %s: %d\n", outcome, o.Outcomes[outcome])
	}
	_, _ = fmt.Fprintf(w, "%s to %s (%.1f days): %d deliveries seen, %d started, %d released, %d dropped: %.1f started and %.1f released a day\n", clock(a.now, o.Since), clock(a.now, o.Until), o.Days, o.Deliveries, o.Started, o.Released, o.Dropped, o.StartedPerDay, o.ReleasedPerDay)
	_, _ = fmt.Fprintf(w, "loop up %.0f%%, down %s", 100*o.LoopUptime, dur(o.LoopDown))
	for i, g := range o.Outages {
		if i == 0 {
			_, _ = fmt.Fprintf(w, "; longest silences:")
		}
		_, _ = fmt.Fprintf(w, " %s to %s (%s)", clock(a.now, g.From), clock(a.now, g.To), dur(g.To.Sub(g.From)))
	}
	_, _ = fmt.Fprintf(w, "\nbusy %.1f of a ceiling of %.0f (%.0f%%)", o.MeanBusy, o.MeanCeiling, 100*o.Utilization)
	for _, p := range o.Providers {
		_, _ = fmt.Fprintf(w, "; %s %.1f of %.0f", p.Provider, p.MeanBusy, p.MeanCeiling)
	}
	_, _ = fmt.Fprintf(w, "\n%d worker and %d reviewer starts, %d merge lanes, %d releases rolled back, %d GitHub limits hit\n", o.WorkerStarts, o.ReviewStarts, o.Lanes, o.Rollbacks, o.RateLimited)
	_, _ = fmt.Fprintf(w, "\nSTEP\tN\tMEDIAN\tP90\tMAX\n")
	for _, s := range o.Stages {
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n", s.Stage, s.Residence.Count, dur(s.Residence.Median), dur(s.Residence.P90), dur(s.Residence.Max))
	}
	section := func(title string, cs []sim.Count) {
		if len(cs) == 0 {
			return
		}
		_, _ = fmt.Fprintf(w, "\n%s\n", title)
		for _, c := range cs {
			_, _ = fmt.Fprintf(w, "%d\t%s\n", c.N, c.Name)
		}
	}
	section("PARKED ON", o.Parks)
	section("MERGES FAILED", o.MergeFailures)
	section("LOOP FAILURES", o.Failures)
	return w.Flush()
}

func (a *app) printComparison(c sim.Comparison) error {
	o := c.Observed
	w := a.table()
	_, _ = fmt.Fprintf(w, "The model against the log, %s to %s (%.1f days)\n\n", clock(a.now, o.Since), clock(a.now, o.Until), o.Days)
	_, _ = fmt.Fprintf(w, "STAGE\tMODEL\tLOG\tN\tGAP\tTHE LOG NAMES\n")
	for _, r := range c.Rows {
		obs, gap := "-", "-"
		if r.Observed.Count > 0 {
			obs, gap = dur(r.Observed.Median), signed(r.Gap)
		}
		cause := ""
		if len(r.Causes) > 0 {
			cause = r.Causes[0]
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", r.Stage, dur(r.Expected.Median), obs, r.Observed.Count, gap, cause)
		for _, more := range r.Causes[min(1, len(r.Causes)):] {
			_, _ = fmt.Fprintf(w, "\t\t\t\t\t%s\n", more)
		}
	}
	_, _ = fmt.Fprintf(w, "loop up\t100%%\t%.0f%%\t\t%s\t", 100*o.LoopUptime, dur(-o.LoopDown))
	for i, g := range o.Outages {
		if i > 0 {
			_, _ = fmt.Fprintf(w, ", ")
		}
		_, _ = fmt.Fprintf(w, "silent %s to %s", clock(a.now, g.From), clock(a.now, g.To))
	}
	_, _ = fmt.Fprintf(w, "\nbusy agents\t%d\t%.1f\t\t%.0f%%\tof a ceiling of %.0f over the window\n", c.Model.Workers, o.MeanBusy, 100*o.Utilization, o.MeanCeiling)
	_, _ = fmt.Fprintf(w, "released a day\t%.0f\t%.0f\t\t\t%d started, %d released, %d dropped (%.0f%% of those that ended); the model with the log's values gives %.0f\n", c.Expected.ReleasedPerDay, o.ReleasedPerDay, o.Started, o.Released, o.Dropped, 100*o.DropShare, c.Baseline.ReleasedPerDay)
	if err := w.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(a.out, "\nOne change at a time, by the released issues a day it gains (the cycle is a start to its release):\n")
	w = a.table()
	_, _ = fmt.Fprintf(w, "  GAIN\tA DAY\tCYCLE\tCHANGE\n")
	for _, x := range c.WhatIfs {
		_, _ = fmt.Fprintf(w, "  %+.0f\t%.0f\t%s\t%s\n", x.Gain, x.ReleasedPerDay, dur(x.Cycle), x.Change)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(a.out, "\n")
	for _, f := range c.Findings {
		_, _ = fmt.Fprintf(a.out, "- %s\n", f)
	}
	return nil
}

func signed(d time.Duration) string {
	if d >= 0 {
		return "+" + dur(d)
	}
	return dur(d)
}
