package sim

import (
	"container/heap"
	"math"
	"math/rand/v2"
	"slices"
	"time"
)

// Quantiles summarise a set of durations.
type Quantiles struct {
	Count  int           `json:"count"`
	Median time.Duration `json:"median"`
	P90    time.Duration `json:"p90"`
	Max    time.Duration `json:"max"`
}

// StageResult is one stage's figures: Wait is the time an issue waited for
// the loop or a resource before the stage, Service the stage's own time
// and Residence both, which is what a delivery.step event measures.
type StageResult struct {
	Stage     string    `json:"stage"`
	Wait      Quantiles `json:"wait"`
	Service   Quantiles `json:"service"`
	Residence Quantiles `json:"residence"`
}

// Result is what a run of the model yields.
type Result struct {
	Horizon        time.Duration `json:"horizon"`
	Started        int           `json:"started"`
	Released       int           `json:"released"`
	Dropped        int           `json:"dropped"`
	Open           int           `json:"open"`
	StartedPerDay  float64       `json:"startedPerDay"`
	ReleasedPerDay float64       `json:"releasedPerDay"`
	// Cycle is the time from a worker's start to the release.
	Cycle Quantiles `json:"cycle"`
	// BacklogWait is the time an issue waited on the board for a worker.
	BacklogWait Quantiles     `json:"backlogWait"`
	Stages      []StageResult `json:"stages"`
	// WorkerUtilization is the busy slot time over the slots (Workers plus
	// ReviewHeadroom) × Horizon, LaneUtilization the merging time over
	// Lanes × Horizon.
	WorkerUtilization float64       `json:"workerUtilization"`
	LaneUtilization   float64       `json:"laneUtilization"`
	LoopDown          time.Duration `json:"loopDown"`
}

// Run simulates the model over its horizon. The same model and seed give
// the same result.
func Run(m Model) Result {
	e := newEngine(m)
	e.run()
	return e.result()
}

type event struct {
	at  time.Duration
	seq int
	fn  func()
}

type eventHeap []*event

func (h eventHeap) Len() int { return len(h) }
func (h eventHeap) Less(i, j int) bool {
	if h[i].at != h[j].at {
		return h[i].at < h[j].at
	}
	return h[i].seq < h[j].seq
}
func (h eventHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *eventHeap) Push(x any)   { *h = append(*h, x.(*event)) }
func (h *eventHeap) Pop() any     { old := *h; x := old[len(old)-1]; *h = old[:len(old)-1]; return x }
func (h *eventHeap) peek() *event { return (*h)[0] }
func (h *eventHeap) empty() bool  { return len(*h) == 0 }
func (h *eventHeap) add(e *event) { heap.Push(h, e) }
func (h *eventHeap) next() *event { return heap.Pop(h).(*event) }

// issue is one issue in flight. Its draws come from a stream of its own,
// seeded by its number, so that a change to one stage's median scales that
// stage's draws and leaves every other draw as it was: two runs that differ
// in one parameter differ in that parameter alone, not in their luck.
type issue struct {
	lane     int
	started  time.Duration
	readyAt  time.Duration
	approved bool
	rng      *rand.Rand
}

type samples struct{ wait, service, residence []time.Duration }

type engine struct {
	m      Model
	rng    *rand.Rand
	now    time.Duration
	seq    int
	events eventHeap

	busy       int
	busyAt     time.Duration
	busyTime   time.Duration
	laneBusy   []bool
	laneAt     []time.Duration
	laneTime   time.Duration
	laneQueues [][]*issue

	loopUp    bool
	loopDown  time.Duration
	loopAt    time.Duration
	backlog   int
	fixQueue  []*issue
	revQueue  []*issue
	byStage   map[string]*samples
	cycles    []time.Duration
	backwaits []time.Duration
	started   int
	released  int
	dropped   int
}

func newEngine(m Model) *engine {
	if m.Workers <= 0 {
		m.Workers = 1
	}
	if m.Lanes <= 0 {
		m.Lanes = 1
	}
	if m.Horizon <= 0 {
		m.Horizon = 24 * time.Hour
	}
	if m.LoopUptime <= 0 || m.LoopUptime > 1 {
		m.LoopUptime = 1
	}
	if m.LoopOutage <= 0 {
		m.LoopOutage = time.Hour
	}
	e := &engine{
		m:          m,
		rng:        rand.New(rand.NewPCG(uint64(m.Seed), uint64(m.Seed)+1)), //nolint:gosec // a deterministic simulation needs a seeded source, not a secure one
		laneBusy:   make([]bool, m.Lanes),
		laneAt:     make([]time.Duration, m.Lanes),
		laneQueues: make([][]*issue, m.Lanes),
		loopUp:     true,
		backlog:    m.Backlog,
		byStage:    map[string]*samples{},
	}
	for _, s := range Stages {
		e.byStage[s] = &samples{}
	}
	e.byStage[StageDecision] = &samples{}
	return e
}

func (e *engine) schedule(after time.Duration, fn func()) {
	e.seq++
	e.events.add(&event{at: e.now + after, seq: e.seq, fn: fn})
}

// sample draws a stage's service time for the issue: lognormal around the
// stage's median.
func (e *engine) sample(i *issue, stage string) time.Duration {
	s, ok := e.m.Stages[stage]
	if !ok || s.Median <= 0 {
		return 0
	}
	if s.Sigma <= 0 {
		return s.Median
	}
	return time.Duration(float64(s.Median) * math.Exp(s.Sigma*i.rng.NormFloat64()))
}

func (e *engine) exponential(mean time.Duration) time.Duration {
	return time.Duration(e.rng.ExpFloat64() * float64(mean))
}

// chance draws the issue's luck for one event; the draw is made whatever
// p is, so that the streams stay aligned between runs.
func (e *engine) chance(i *issue, p float64) bool { return i.rng.Float64() < p }

func (e *engine) run() {
	if e.m.ArrivalsPerDay > 0 {
		e.schedule(e.exponential(time.Duration(float64(24*time.Hour)/e.m.ArrivalsPerDay)), e.arrive)
	}
	if e.m.LoopUptime < 1 {
		e.schedule(e.exponential(e.upLength()), e.loopGoesDown)
	}
	e.admit()
	for !e.events.empty() && e.events.peek().at <= e.m.Horizon {
		ev := e.events.next()
		e.now = ev.at
		ev.fn()
	}
	e.now = e.m.Horizon
	e.account()
	if !e.loopUp {
		e.loopDown += e.now - e.loopAt
	}
}

func (e *engine) upLength() time.Duration {
	return time.Duration(float64(e.m.LoopOutage) * e.m.LoopUptime / (1 - e.m.LoopUptime))
}

func (e *engine) loopGoesDown() {
	e.loopUp = false
	e.loopAt = e.now
	e.schedule(e.exponential(e.m.LoopOutage), e.loopComesUp)
}

func (e *engine) loopComesUp() {
	e.loopUp = true
	e.loopDown += e.now - e.loopAt
	e.schedule(e.exponential(e.upLength()), e.loopGoesDown)
	e.admit()
	for lane := range e.laneQueues {
		e.mergeNext(lane)
	}
}

func (e *engine) arrive() {
	if e.backlog >= 0 {
		e.backlog++
	}
	e.schedule(e.exponential(time.Duration(float64(24*time.Hour)/e.m.ArrivalsPerDay)), e.arrive)
	e.admit()
}

// account integrates the busy slots and lanes up to now.
func (e *engine) account() {
	e.busyTime += time.Duration(e.busy) * (e.now - e.busyAt)
	e.busyAt = e.now
	for i, b := range e.laneBusy {
		if b {
			e.laneTime += e.now - e.laneAt[i]
			e.laneAt[i] = e.now
		}
	}
}

func (e *engine) take() { e.account(); e.busy++ }
func (e *engine) free() { e.account(); e.busy--; e.admit() }

// admit starts what the loop would start now: reviewers first, within the
// review headroom, then fixers, then new issues from the board.
func (e *engine) admit() {
	if !e.loopUp {
		return
	}
	for len(e.revQueue) > 0 && e.busy < e.m.Workers+e.m.ReviewHeadroom {
		i := e.revQueue[0]
		e.revQueue = e.revQueue[1:]
		e.startReview(i)
	}
	for len(e.fixQueue) > 0 && e.busy < e.m.Workers {
		i := e.fixQueue[0]
		e.fixQueue = e.fixQueue[1:]
		e.startFix(i)
	}
	for e.busy < e.m.Workers && e.backlog != 0 {
		if e.backlog > 0 {
			e.backlog--
		}
		e.startIssue()
	}
}

func (e *engine) startIssue() {
	e.started++
	n := uint64(e.started)                                                                                    //nolint:gosec // a non-negative count
	i := &issue{lane: e.started % e.m.Lanes, started: e.now, rng: rand.New(rand.NewPCG(uint64(e.m.Seed), n))} //nolint:gosec // a deterministic simulation needs a seeded source, not a secure one
	e.backwaits = append(e.backwaits, 0)
	e.take()
	d := e.sample(i, StageImplementing)
	e.record(StageImplementing, 0, d)
	e.schedule(d, func() { e.implemented(i) })
}

func (e *engine) implemented(i *issue) {
	if e.chance(i, e.m.Dropped) {
		e.dropped++
		e.free()
		return
	}
	e.free()
	if e.chance(i, e.m.DecisionShare) {
		d := e.sample(i, StageDecision)
		e.record(StageDecision, 0, d)
		e.schedule(d, func() { e.runCI(i) })
		return
	}
	e.runCI(i)
}

func (e *engine) runCI(i *issue) {
	d := e.sample(i, StageCI)
	if e.m.UpdateBranch && i.approved {
		// The merge's own CI run is counted in merging.
		d = 0
	}
	e.record(StageCI, 0, d)
	e.schedule(d, func() {
		if e.chance(i, e.m.RedCI) {
			i.readyAt = e.now
			e.fixQueue = append(e.fixQueue, i)
			e.admit()
			return
		}
		if i.approved {
			e.queueMerge(i)
			return
		}
		i.readyAt = e.now
		e.revQueue = append(e.revQueue, i)
		e.admit()
	})
}

func (e *engine) startReview(i *issue) {
	// The loop's own latency between green CI and the reviewer's start.
	wait := e.now - i.readyAt
	d := e.sample(i, StageReviewQueued)
	e.record(StageReviewQueued, wait, d)
	e.take()
	e.schedule(d, func() {
		s := e.sample(i, StageReviewing)
		e.record(StageReviewing, 0, s)
		e.schedule(s, func() {
			i.approved = true
			e.free()
			if e.chance(i, e.m.ChangesRequested) {
				i.readyAt = e.now
				e.fixQueue = append(e.fixQueue, i)
				e.admit()
				return
			}
			e.queueMerge(i)
		})
	})
}

func (e *engine) startFix(i *issue) {
	wait := e.now - i.readyAt
	d := e.sample(i, StageFixing)
	e.record(StageFixing, wait, d)
	e.take()
	e.schedule(d, func() {
		e.free()
		e.runCI(i)
	})
}

func (e *engine) queueMerge(i *issue) {
	i.readyAt = e.now
	e.laneQueues[i.lane] = append(e.laneQueues[i.lane], i)
	e.mergeNext(i.lane)
}

func (e *engine) mergeNext(lane int) {
	if !e.loopUp || e.laneBusy[lane] || len(e.laneQueues[lane]) == 0 {
		return
	}
	i := e.laneQueues[lane][0]
	e.laneQueues[lane] = e.laneQueues[lane][1:]
	e.account()
	e.laneBusy[lane] = true
	e.laneAt[lane] = e.now
	d := e.sample(i, StageMerging)
	if e.m.UpdateBranch {
		d += e.sample(i, StageCI)
	}
	e.record(StageMerging, e.now-i.readyAt, d)
	e.schedule(d, func() {
		e.account()
		e.laneBusy[lane] = false
		e.mergeNext(lane)
		r := e.sample(i, StageReleasing)
		e.record(StageReleasing, 0, r)
		e.schedule(r, func() {
			e.released++
			e.cycles = append(e.cycles, e.now-i.started)
		})
	})
}

func (e *engine) record(stage string, wait, service time.Duration) {
	s := e.byStage[stage]
	s.wait = append(s.wait, wait)
	s.service = append(s.service, service)
	s.residence = append(s.residence, wait+service)
}

func quantiles(ds []time.Duration) Quantiles {
	if len(ds) == 0 {
		return Quantiles{}
	}
	ds = slices.Clone(ds)
	slices.Sort(ds)
	median := ds[len(ds)/2]
	if len(ds)%2 == 0 {
		median = ds[len(ds)/2-1] + (median-ds[len(ds)/2-1])/2
	}
	return Quantiles{Count: len(ds), Median: median, P90: ds[min(len(ds)-1, len(ds)*9/10)], Max: ds[len(ds)-1]}
}

func (e *engine) result() Result {
	days := e.m.Horizon.Hours() / 24
	r := Result{
		Horizon:        e.m.Horizon,
		Started:        e.started,
		Released:       e.released,
		Dropped:        e.dropped,
		Open:           e.started - e.released - e.dropped,
		StartedPerDay:  float64(e.started) / days,
		ReleasedPerDay: float64(e.released) / days,
		Cycle:          quantiles(e.cycles),
		BacklogWait:    quantiles(e.backwaits),
		LoopDown:       e.loopDown,
	}
	if total := float64(e.m.Workers+e.m.ReviewHeadroom) * float64(e.m.Horizon); total > 0 {
		r.WorkerUtilization = float64(e.busyTime) / total
	}
	if total := float64(e.m.Lanes) * float64(e.m.Horizon); total > 0 {
		r.LaneUtilization = float64(e.laneTime) / total
	}
	for _, name := range append(slices.Clone(Stages), StageDecision) {
		s := e.byStage[name]
		if len(s.residence) == 0 {
			continue
		}
		r.Stages = append(r.Stages, StageResult{Stage: name, Wait: quantiles(s.wait), Service: quantiles(s.service), Residence: quantiles(s.residence)})
	}
	return r
}
