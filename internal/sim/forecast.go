package sim

import (
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// States of an item's agent on the roster.
const (
	AgentBusy   = "busy"
	AgentParked = "parked"
	AgentIdle   = "idle"
)

// Item is one active delivery as the loop records it.
type Item struct {
	Ref   string    `json:"ref"`
	Pulls []string  `json:"pulls,omitempty"`
	Step  string    `json:"step"`
	Since time.Time `json:"since"`
	Agent string    `json:"agent,omitempty"`
	// AgentState is busy, parked, idle or "" for no agent on the roster.
	AgentState string `json:"agentState,omitempty"`
	Waits      string `json:"waits,omitempty"`
}

// BoardItem is a board candidate in the order board next walks them;
// Skip is the implementation's reason not to start it, "" when free.
type BoardItem struct {
	Ref    string `json:"ref"`
	Status string `json:"status,omitempty"`
	Skip   string `json:"skip,omitempty"`
}

// ProviderRoom is a provider's busy agents, ceiling and room now.
type ProviderRoom struct {
	Name    string `json:"name"`
	Busy    int    `json:"busy"`
	Ceiling int    `json:"ceiling"`
	Room    int    `json:"room"`
}

// LaneState is a merge in a lane.
type LaneState struct {
	Lane  string    `json:"lane"`
	Ref   string    `json:"ref"`
	Phase string    `json:"phase"`
	Since time.Time `json:"since"`
}

// FailedMerge is a merge the gate recorded as failed.
type FailedMerge struct {
	Ref    string    `json:"ref"`
	At     time.Time `json:"at"`
	Exit   int       `json:"exit"`
	Reason string    `json:"reason,omitempty"`
}

// ParkedAgent is an agent kept parked and what it waits on.
type ParkedAgent struct {
	Name  string `json:"name"`
	On    string `json:"on,omitempty"`
	Waits string `json:"waits"`
}

// Snapshot is the desk now, as the forecast reads it from the state and
// the implementation's own verdicts (capacity, board next).
type Snapshot struct {
	At          time.Time `json:"at"`
	LoopRunning bool      `json:"loopRunning"`
	// Stopped names a standing stop or starts hold.
	Stopped string `json:"stopped,omitempty"`
	// Room and Blocks are capacity's verdict for a worker start.
	Room int `json:"room"`
	// StartBlocked excludes temporary occupancy of worker slots.
	StartBlocked bool              `json:"startBlocked,omitempty"`
	MergeBlocks  map[string]string `json:"mergeBlocks,omitempty"`
	Blocks       []string          `json:"blocks,omitempty"`
	Busy         int               `json:"busy"`
	Parked       int               `json:"parked"`
	Idle         int               `json:"idle"`
	Providers    []ProviderRoom    `json:"providers,omitempty"`
	// Reviewers names the busy review agents.
	Reviewers      []string              `json:"reviewers,omitempty"`
	Items          []Item                `json:"items"`
	Board          []BoardItem           `json:"board,omitempty"`
	BoardRead      bool                  `json:"boardRead"`
	Holds          []string              `json:"holds,omitempty"`
	Lanes          []LaneState           `json:"lanes,omitempty"`
	ReviewRequests []state.ReviewRequest `json:"reviewRequests,omitempty"`
	FailedMerges   []FailedMerge         `json:"failedMerges,omitempty"`
	ParkedAgents   []ParkedAgent         `json:"parkedAgents,omitempty"`
	Timers         []int                 `json:"timers,omitempty"`
	// UnmergedLimit is supervisor.unmergedLimit, for the record: board
	// next applies it and holds the items past it.
	UnmergedLimit int `json:"unmergedLimit"`
}

// Transition is a stage an item is forecast to reach at a time.
type Transition struct {
	Step string    `json:"step"`
	At   time.Time `json:"at"`
}

// ItemForecast is one item's forecast path.
type ItemForecast struct {
	Ref   string    `json:"ref"`
	Step  string    `json:"step"`
	Since time.Time `json:"since"`
	// Timeline is every stage the item is forecast to enter within the
	// horizon, done last when it is released.
	Timeline []Transition `json:"timeline,omitempty"`
	// ReleaseAt is zero while the release lies past the horizon.
	ReleaseAt time.Time `json:"releaseAt,omitzero"`
	// Stuck says why the item is forecast not to move at all.
	Stuck string `json:"stuck,omitempty"`
}

// StartForecast is a board item forecast to start.
type StartForecast struct {
	Ref string    `json:"ref"`
	At  time.Time `json:"at"`
}

// Total is the forecast count after a time.
type Total struct {
	After    time.Duration `json:"after"`
	Started  int           `json:"started"`
	Released int           `json:"released"`
}

// Problem is what the snapshot says will not move, or moves too slowly,
// before any simulation: Stop stops the flow, Slow slows it.
type Problem struct {
	Severity string   `json:"severity"`
	What     string   `json:"what"`
	Refs     []string `json:"refs,omitempty"`
}

const (
	SeverityStop = "stop"
	SeveritySlow = "slow"
)

// Forecast is the desk's expected flow over a horizon from a snapshot.
type Forecast struct {
	At       time.Time       `json:"at"`
	Horizon  time.Duration   `json:"horizon"`
	Model    Model           `json:"model"`
	Snapshot Snapshot        `json:"snapshot"`
	Items    []ItemForecast  `json:"items"`
	Starts   []StartForecast `json:"starts,omitempty"`
	Totals   []Total         `json:"totals"`
	Problems []Problem       `json:"problems,omitempty"`
}

// stuckAfter is how long an item may sit in a step before the forecast
// calls it stuck, per step: the model's expectation with wide room.
var stuckAfter = map[string]time.Duration{
	StageImplementing: 6 * time.Hour,
	StageCI:           time.Hour,
	StageReviewQueued: time.Hour,
	StageReviewing:    time.Hour,
	StageFixing:       6 * time.Hour,
	StageMerging:      2 * time.Hour,
	StageReleasing:    12 * time.Hour,
}

// Forecasting runs the snapshot forward through the model over the
// horizon: the items in flight from their current steps, the free board
// items as the slots allow, under the implementation's own verdicts
// (loop running, room, holds, reviews already requested).
func Forecasting(m Model, s Snapshot, horizon time.Duration) Forecast {
	f := Forecast{At: s.At, Horizon: horizon, Model: m, Snapshot: s}
	f.Problems = problems(s)
	stuck := stuckRefs(f.Problems)
	m.Horizon = horizon
	m.Backlog = 0
	m.ArrivalsPerDay = 0
	m.Dropped = 0
	if slots := s.Busy + s.Room; slots > 0 {
		m.Workers = slots
	}
	if !s.LoopRunning || s.Stopped != "" {
		m.LoopUptime = 1
	}
	e := newEngine(m)
	e.now = 0
	e.loopUp = s.LoopRunning && s.Stopped == ""
	e.forecastAt = s.At
	e.stopped = s.Stopped
	if !e.loopUp {
		// Nothing moves without the loop: the worker's turn ends in a
		// push the loop never picks up.
		for _, it := range s.Items {
			f.Items = append(f.Items, ItemForecast{Ref: it.Ref, Step: it.Step, Since: it.Since, Stuck: e.whyStill(it)})
		}
		f.Totals = totals(f, s.At, horizon)
		return f
	}
	e.paths = map[string][]Transition{}
	e.mergeBlocks = s.MergeBlocks
	lanes := map[string]int{}
	lane := func(ref string) int {
		repo := repoOf(ref)
		if _, ok := lanes[repo]; !ok {
			lanes[repo] = len(lanes)
		}
		return lanes[repo]
	}
	for _, it := range s.Items {
		lane(it.Ref)
	}
	for _, b := range s.Board {
		lane(b.Ref)
	}
	e.resizeLanes(max(1, len(lanes)))
	e.lanes = lanes
	// The slots the busy agents hold now are taken by the items they serve.
	for _, it := range s.Items {
		if why, ok := stuck[it.Ref]; ok {
			e.paths[it.Ref] = nil
			f.Items = append(f.Items, ItemForecast{Ref: it.Ref, Step: it.Step, Since: it.Since, Stuck: why})
			continue
		}
		e.seed(it, lane(it.Ref))
	}
	// The busy agents no delivery explains (filers, dispatchers, workers
	// before their first push) hold their slots for an implementing turn.
	for k, n := 0, s.Busy-e.busy; k < n; k++ {
		e.take()
		e.schedule(e.m.Stages[StageImplementing].Median*time.Duration(2*k+1)/time.Duration(n), e.free)
	}
	// The free board items start as the slots allow, in the board's order.
	free := s.Free()
	if s.StartBlocked {
		free = nil
	}
	e.backlogRefs = append(e.backlogRefs, free...)
	e.backlog = len(free)
	starts := len(free)
	e.run()
	for _, it := range s.Items {
		if _, ok := stuck[it.Ref]; ok {
			continue
		}
		path := e.paths[it.Ref]
		fc := ItemForecast{Ref: it.Ref, Step: it.Step, Since: it.Since, Timeline: path}
		if n := len(path); n > 0 && path[n-1].Step == state.DeliveryDone {
			fc.ReleaseAt = path[n-1].At
		}
		if why := s.MergeBlocks[it.Ref]; why != "" && fc.ReleaseAt.IsZero() {
			fc.Stuck = why
		}
		if len(path) == 0 && fc.Stuck == "" {
			fc.Stuck = e.whyStill(it)
		}
		f.Items = append(f.Items, fc)
	}
	for _, ref := range e.backlogRefs {
		if path := e.paths[ref]; len(path) > 0 {
			f.Starts = append(f.Starts, StartForecast{Ref: ref, At: path[0].At})
			fc := ItemForecast{Ref: ref, Step: StepBoard, Since: s.At, Timeline: path}
			if n := len(path); path[n-1].Step == state.DeliveryDone {
				fc.ReleaseAt = path[n-1].At
			}
			if why := s.MergeBlocks[ref]; why != "" && fc.ReleaseAt.IsZero() {
				fc.Stuck = why
			}
			f.Items = append(f.Items, fc)
		}
	}
	f.Totals = totals(f, s.At, horizon)
	if starts > 0 && len(f.Starts) == 0 && e.loopUp {
		f.Problems = append(f.Problems, Problem{Severity: SeverityStop, What: fmt.Sprintf("%s and no start within %s: every slot stays taken by the items in flight", plural(starts, "free board item"), short(horizon))})
	}
	return f
}

// Free is the board items the loop would start, in the board's order: not
// held by the board, and not in delivery already.
func (s Snapshot) Free() []string {
	inFlight := map[string]bool{}
	for _, it := range s.Items {
		inFlight[strings.ToLower(it.Ref)] = true
	}
	var free []string
	for _, b := range s.Board {
		if b.Skip == "" && !inFlight[strings.ToLower(b.Ref)] {
			free = append(free, b.Ref)
		}
	}
	return free
}

// totals counts the forecast starts and releases by 6h, 12h, 24h and the
// horizon.
func totals(f Forecast, at time.Time, horizon time.Duration) []Total {
	var out []Total
	for _, after := range []time.Duration{6 * time.Hour, 12 * time.Hour, 24 * time.Hour, horizon} {
		if after > horizon || len(out) > 0 && out[len(out)-1].After == after {
			continue
		}
		t := Total{After: after}
		until := at.Add(after)
		for _, st := range f.Starts {
			if !st.At.After(until) {
				t.Started++
			}
		}
		for _, it := range f.Items {
			if !it.ReleaseAt.IsZero() && !it.ReleaseAt.After(until) {
				t.Released++
			}
		}
		out = append(out, t)
	}
	return out
}

// problems reads the snapshot for what will not move before any run.
func problems(s Snapshot) []Problem {
	var out []Problem
	add := func(sev, what string, refs ...string) {
		sort.Strings(refs)
		out = append(out, Problem{Severity: sev, What: what, Refs: refs})
	}
	if !s.LoopRunning {
		add(SeverityStop, "the supervisor loop is not running: nothing starts, is reviewed or merges")
	}
	if s.Stopped != "" {
		add(SeverityStop, "starts are stopped: "+s.Stopped)
	}
	free := s.Free()
	if s.StartBlocked {
		add(SeverityStop, fmt.Sprintf("no start while %s wait: %s", plural(len(free), "free board item"), strings.Join(s.Blocks, "; ")), free...)
	}
	for _, p := range s.Providers {
		if p.Ceiling == 0 {
			add(SeveritySlow, fmt.Sprintf("provider %s has a ceiling of 0: no start on it", p.Name))
		}
	}
	for _, h := range s.Holds {
		add(SeverityStop, "held: "+h)
	}
	// A review already requested at a head while no reviewer runs never
	// starts again (the loop refuses "already requested this head").
	reviewed := map[string]bool{}
	for _, r := range s.ReviewRequests {
		reviewed[strings.ToLower(r.Ref)] = true
	}
	var stale []string
	for _, it := range s.Items {
		if it.Step != StageReviewQueued {
			continue
		}
		for _, p := range it.Pulls {
			if reviewed[strings.ToLower(p)] && !reviewerRuns(s.Reviewers, p) {
				stale = append(stale, it.Ref)
			}
		}
	}
	if len(stale) > 0 {
		add(SeverityStop, "a review is recorded as requested while no reviewer runs: the loop refuses to start one (stale request or worktree)", stale...)
	}
	// Failed merges hold their pull request until the cause is fixed.
	recent := s.At.Add(-4 * time.Hour)
	failed := map[string]FailedMerge{}
	for _, fm := range s.FailedMerges {
		if fm.At.After(recent) {
			failed[strings.ToLower(fm.Ref)] = fm
		}
	}
	for _, it := range s.Items {
		if it.Step != StageMerging && it.Step != StageReviewQueued {
			continue
		}
		for _, p := range it.Pulls {
			if fm, ok := failed[strings.ToLower(p)]; ok {
				add(SeveritySlow, fmt.Sprintf("the merge of %s failed at %s with exit %d%s; the loop retries with backoff up to 1h", p, fm.At.Format("15:04"), fm.Exit, reasonSuffix(fm.Reason)), it.Ref)
			}
		}
	}
	// Items long in a step, with the agent's state.
	byStep := map[string][]string{}
	whys := map[string]string{}
	for _, it := range s.Items {
		limit, ok := stuckAfter[it.Step]
		if !ok || s.At.Sub(it.Since) < limit {
			continue
		}
		byStep[it.Step] = append(byStep[it.Step], it.Ref)
		switch it.AgentState {
		case AgentParked:
			whys[it.Step] = "its worker parked"
		case "", AgentIdle:
			if it.Step == StageImplementing || it.Step == StageFixing {
				whys[it.Step] = "no worker on it"
			}
		}
	}
	for _, step := range Stages {
		refs := byStep[step]
		if len(refs) == 0 {
			continue
		}
		what := fmt.Sprintf("%s in %s for over %s", plural(len(refs), "item"), step, short(stuckAfter[step]))
		if why := whys[step]; why != "" {
			what += " with " + why
		}
		add(SeveritySlow, what, refs...)
	}
	// Parked on a timer that does not exist.
	timers := map[int]bool{}
	for _, id := range s.Timers {
		timers[id] = true
	}
	var orphans []string
	for _, p := range s.ParkedAgents {
		if id, ok := timerIn(p.Waits); ok && !timers[id] {
			orphans = append(orphans, p.Name)
		}
	}
	if len(orphans) > 0 {
		add(SeverityStop, "parked on a timer that no longer exists: never woken", orphans...)
	}
	if s.Idle > 0 && len(free) > 0 {
		add(SeveritySlow, fmt.Sprintf("%s while %s wait", plural(s.Idle, "idle agent"), plural(len(free), "free board item")))
	}
	return out
}

// plural is "1 item" or "3 items".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func reasonSuffix(r string) string {
	if r == "" {
		return ""
	}
	if len(r) > 100 {
		r = r[:100] + "…"
	}
	return " (" + r + ")"
}

func reviewerRuns(reviewers []string, pull string) bool {
	repo, n, _ := strings.Cut(pull, "#")
	short := repo[strings.LastIndex(repo, "/")+1:] + " " + n
	for _, r := range reviewers {
		if strings.Contains(r, short) {
			return true
		}
	}
	return false
}

// timerIn reads "timer 88" or "Timer302" out of a park's wait.
func timerIn(waits string) (int, bool) {
	w := strings.ToLower(waits)
	i := strings.Index(w, "timer")
	if i < 0 {
		return 0, false
	}
	rest := strings.TrimLeft(w[i+5:], " #:")
	n := 0
	for _, c := range rest {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n, n > 0
}

// stuckRefs maps every item a stop problem names to its reason.
func stuckRefs(ps []Problem) map[string]string {
	out := map[string]string{}
	for _, p := range ps {
		if p.Severity != SeverityStop {
			continue
		}
		for _, r := range p.Refs {
			if _, ok := out[r]; !ok {
				out[r] = p.What
			}
		}
	}
	return out
}

func repoOf(ref string) string {
	repo, _, _ := strings.Cut(ref, "#")
	return repo
}

// seed places an active item in the engine at its step with the time it
// has already spent there.
func (e *engine) seed(it Item, lane int) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.ToLower(it.Ref)))
	i := &issue{ref: it.Ref, lane: lane, started: 0, rng: rand.New(rand.NewPCG(uint64(e.m.Seed), h.Sum64()))} //nolint:gosec // a deterministic simulation needs a seeded source, not a secure one
	elapsed := e.forecastAt.Sub(it.Since)
	// What is left of a stage: its median less the time spent, and half
	// a median once that is used up (the spread is wide).
	remaining := func(stage string) time.Duration {
		median := e.m.Stages[stage].Median
		return max(median/2, median-elapsed)
	}
	working := it.AgentState == AgentBusy
	switch it.Step {
	case StageImplementing, StageFixing:
		if !working {
			// A parked or absent worker pushes nothing until it is woken.
			return
		}
		e.take()
		e.schedule(remaining(it.Step), func() {
			e.free()
			e.enter(i, StageCI)
			e.runCI(i)
		})
	case StageCI:
		e.schedule(remaining(StageCI), func() { e.afterCI(i) })
	case StageReviewQueued:
		i.readyAt = -elapsed
		e.revQueue = append(e.revQueue, i)
	case StageReviewing:
		e.take()
		e.schedule(remaining(StageReviewing), func() { e.reviewed(i) })
	case StageMerging:
		if e.mergeBlocks[it.Ref] != "" {
			return
		}
		i.approved = true
		i.readyAt = -elapsed
		e.laneQueues[lane] = append(e.laneQueues[lane], i)
	case StageReleasing:
		e.schedule(remaining(StageReleasing), func() { e.release(i) })
	default:
		return
	}
}

// whyStill says why an in-flight item did not move in the run.
func (e *engine) whyStill(it Item) string {
	switch {
	case !e.loopUp:
		if e.stopped != "" {
			return "starts are stopped: " + e.stopped
		}
		return "the loop is not running"
	case (it.Step == StageImplementing || it.Step == StageFixing) && it.AgentState != AgentBusy:
		if it.Waits != "" {
			return "its worker is " + orNone(it.AgentState) + ": " + it.Waits
		}
		return "its worker is " + orNone(it.AgentState)
	case it.Step == StageReviewQueued:
		return "no reviewer slot came free within the horizon"
	case it.Step == StageMerging:
		return "the lane did not come free within the horizon"
	}
	return "did not finish within the horizon"
}

func orNone(s string) string {
	if s == "" {
		return "not on the roster"
	}
	return s
}
