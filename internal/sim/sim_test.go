package sim

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

const testRef = "o/r#1"

// TestRunIsDeterministic: the same model and seed give the same result.
func TestRunIsDeterministic(t *testing.T) {
	m := Default()
	m.Horizon = 2 * 24 * time.Hour
	a, b := Run(m), Run(m)
	if a.Released == 0 || a.Released != b.Released || a.Cycle.Median != b.Cycle.Median {
		t.Fatalf("runs differ: %d/%d released, %s/%s cycle", a.Released, b.Released, a.Cycle.Median, b.Cycle.Median)
	}
	m.Seed = 2
	if c := Run(m); c.Released == a.Released && c.Cycle.Median == a.Cycle.Median {
		t.Fatalf("another seed gives the same run")
	}
}

// TestRunCapacityBoundsThroughput: with the board never running out, the
// released issues a day follow the slots, and every slot is busy.
func TestRunCapacityBoundsThroughput(t *testing.T) {
	m := Default()
	m.Horizon = 3 * 24 * time.Hour
	m.Lanes = 10
	ten := Run(m)
	m.Workers = 20
	twenty := Run(m)
	if ten.WorkerUtilization < 0.8 || twenty.WorkerUtilization < 0.85 {
		t.Fatalf("slots idle with a full board: %.2f %.2f", ten.WorkerUtilization, twenty.WorkerUtilization)
	}
	if ratio := twenty.ReleasedPerDay / ten.ReleasedPerDay; ratio < 1.6 || ratio > 2.4 {
		t.Fatalf("twice the slots release %.1f times as much (%.1f vs %.1f a day)", ratio, twenty.ReleasedPerDay, ten.ReleasedPerDay)
	}
}

// TestRunArrivalsBoundThroughput: with a board fed at a rate below the
// capacity, that rate is the throughput and the slots stay partly idle.
func TestRunArrivalsBoundThroughput(t *testing.T) {
	m := Default()
	m.Backlog = 0
	m.ArrivalsPerDay = 40
	m.Horizon = 10 * 24 * time.Hour
	r := Run(m)
	if r.ReleasedPerDay < 34 || r.ReleasedPerDay > 44 {
		t.Fatalf("released %.1f a day for 40 arrivals", r.ReleasedPerDay)
	}
	if r.WorkerUtilization > 0.6 {
		t.Fatalf("slots %.0f%% busy on 40 arrivals a day", 100*r.WorkerUtilization)
	}
}

// TestRunLoopDownStopsTheFlow: a loop down half the time halves the
// throughput roughly, and the down time is accounted.
func TestRunLoopDownStopsTheFlow(t *testing.T) {
	m := Default()
	m.Horizon = 7 * 24 * time.Hour
	up := Run(m)
	m.LoopUptime = 0.5
	m.LoopOutage = 2 * time.Hour
	half := Run(m)
	if down := half.LoopDown.Hours() / m.Horizon.Hours(); down < 0.35 || down > 0.65 {
		t.Fatalf("loop down %.0f%% at an uptime of 50%%", 100*down)
	}
	if half.ReleasedPerDay > 0.75*up.ReleasedPerDay {
		t.Fatalf("a loop down half the time released %.0f a day against %.0f", half.ReleasedPerDay, up.ReleasedPerDay)
	}
}

// TestRunSerialLaneIsTheBottleneck: one lane with a long merge bounds the
// throughput at the lane's pace whatever the slots.
func TestRunSerialLaneIsTheBottleneck(t *testing.T) {
	m := Default().With(StageMerging, 40*time.Minute)
	m.Lanes = 1
	m.Workers = 20
	m.Horizon = 3 * 24 * time.Hour
	r := Run(m)
	if r.ReleasedPerDay > 40 || r.LaneUtilization < 0.9 {
		t.Fatalf("one lane at 40m merged %.0f a day, lane %.0f%% busy", r.ReleasedPerDay, 100*r.LaneUtilization)
	}
	var merging StageResult
	for _, s := range r.Stages {
		if s.Stage == StageMerging {
			merging = s
		}
	}
	if merging.Wait.Median < time.Hour {
		t.Fatalf("merges waited %s for the lane", merging.Wait.Median)
	}
}

// TestRunDroppedAndUpdateBranch: dropped work never releases, and
// --update-branch adds a CI run to every merge.
func TestRunDroppedAndUpdateBranch(t *testing.T) {
	m := Default()
	m.Horizon = 2 * 24 * time.Hour
	m.Dropped = 0.5
	r := Run(m)
	if r.Dropped == 0 || float64(r.Dropped)/float64(r.Started) < 0.35 {
		t.Fatalf("%d of %d dropped at a chance of 50%%", r.Dropped, r.Started)
	}
	m.Dropped = 0
	plain := Run(m)
	m.UpdateBranch = true
	updated := Run(m)
	var a, b StageResult
	for _, s := range plain.Stages {
		if s.Stage == StageMerging {
			a = s
		}
	}
	for _, s := range updated.Stages {
		if s.Stage == StageMerging {
			b = s
		}
	}
	if b.Service.Median < a.Service.Median+10*time.Minute {
		t.Fatalf("--update-branch merges take %s against %s", b.Service.Median, a.Service.Median)
	}
}

func step(at time.Time, ref, step string, since, started time.Time) state.Event {
	raw, _ := json.Marshal(state.Delivery{Ref: ref, Pulls: []string{}, Step: step, Since: since, Started: started})
	return state.Event{At: at, By: state.Party{Name: "supervisor loop"}, Verb: state.DeliveryStepVerb, Subject: ref, Detail: string(raw)}
}

func loopEvent(at time.Time) state.Event {
	return state.Event{At: at, By: state.Party{Name: "supervisor loop"}, Verb: "supervisor.claim", Detail: "[go o/r#1@abc] w"}
}

// TestObserve reads a synthetic week: the steps' residence, the drops and
// releases, the loop's silence, the busy agents, the parks and failures.
func TestObserve(t *testing.T) {
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(2 * 24 * time.Hour)
	t0 := since.Add(time.Hour)
	var events []state.Event
	// o/r#1: implementing 30m, ci 20m, review-queued 40m, reviewing 10m, merging 30m, releasing 8h, done.
	steps := []struct {
		step string
		d    time.Duration
	}{{StageImplementing, 30 * time.Minute}, {StageCI, 20 * time.Minute}, {StageReviewQueued, 40 * time.Minute}, {StageReviewing, 10 * time.Minute}, {StageMerging, 30 * time.Minute}, {StageReleasing, 8 * time.Hour}}
	at := t0
	for _, s := range steps {
		events = append(events, step(at, testRef, s.step, at, t0))
		at = at.Add(s.d)
	}
	events = append(events, step(at, testRef, state.DeliveryDone, at, t0))
	// o/r#2 and o/r#3 dropped, o/r#3 before the window (does not count).
	events = append(events, step(t0, "o/r#2", StageImplementing, t0, t0), step(t0.Add(time.Hour), "o/r#2", state.DeliveryDropped, t0.Add(time.Hour), t0))
	events = append(events, step(since.Add(-2*time.Hour), "o/r#3", StageImplementing, since.Add(-2*time.Hour), since.Add(-2*time.Hour)), step(since.Add(-time.Hour), "o/r#3", state.DeliveryDropped, since.Add(-time.Hour), since.Add(-2*time.Hour)))
	// The loop: an event every 5 minutes for the first day, then silence.
	for at := since; at.Before(since.Add(24 * time.Hour)); at = at.Add(5 * time.Minute) {
		events = append(events, loopEvent(at))
	}
	// USAGE every half hour: 2 busy of 10 for a day, then 0 of 10.
	for at := since; at.Before(until); at = at.Add(30 * time.Minute) {
		busy := 2
		if !at.Before(since.Add(24 * time.Hour)) {
			busy = 0
		}
		events = append(events, state.Event{At: at, Verb: "watch.usage", Detail: fmt.Sprintf("USAGE: codex: %d busy of ceiling 10, usage 7d 30%%", busy)})
	}
	events = append(events,
		state.Event{At: t0, Verb: "agents.start", Detail: "w: kind worker, harness codex"},
		state.Event{At: t0, Verb: "agents.start", Detail: "r: kind review, harness codex"},
		state.Event{At: t0, Verb: "agents.park", Detail: "w: parked on o/r#9: desk review"},
		state.Event{At: t0, Verb: "agents.park", Detail: "w: parked: CI for https://x/pull/1"},
		state.Event{At: t0, Verb: "merge.failed", Detail: "o/r#5 exit 3, nothing merged: conflicts"},
		state.Event{At: t0, Verb: "merging", Detail: "o/r#5 in lane o/r"},
		state.Event{At: t0, Verb: "merging", Detail: "o/s#5 in lane o/s"},
		state.Event{At: t0, Verb: "supervisor.failure", Detail: `[review o/r#7@abc] {"message":"o/r#7 already requested this head or review round","count":3}`},
		state.Event{At: t0, Verb: "supervisor.failure", Detail: `[review o/r#8@def] {"message":"o/r#8 already requested this head or review round","count":1}`},
		state.Event{At: t0, Verb: "self-update.recovery", Detail: "rolled back to rc.1"},
		state.Event{At: t0, Verb: "supervisor.file", Detail: "[recovery x] supervisor loop reported; file: RELEASE ROLLED BACK: v1 failed"},
	)
	o := Observe(events, since, until)
	if o.Started != 2 || o.Released != 1 || o.Dropped != 1 || o.Deliveries != 3 || o.DropShare != 0.5 {
		t.Fatalf("started %d released %d dropped %d of %d", o.Started, o.Released, o.Dropped, o.Deliveries)
	}
	want := map[string]time.Duration{StageImplementing: 30 * time.Minute, StageCI: 20 * time.Minute, StageReviewQueued: 40 * time.Minute, StageReviewing: 10 * time.Minute, StageMerging: 30 * time.Minute, StageReleasing: 8 * time.Hour}
	for _, s := range o.Stages {
		if s.Stage == StageImplementing {
			// o/r#2's hour and o/r#1's half hour: the median of two is their mean.
			want[StageImplementing] = 45 * time.Minute
		}
		if s.Residence.Median != want[s.Stage] {
			t.Errorf("%s: residence %s, want %s", s.Stage, s.Residence.Median, want[s.Stage])
		}
	}
	if len(o.Stages) != len(want) {
		t.Errorf("stages: %+v", o.Stages)
	}
	if o.LoopUptime < 0.49 || o.LoopUptime > 0.51 || len(o.Outages) != 1 {
		t.Errorf("loop up %.2f, outages %+v", o.LoopUptime, o.Outages)
	}
	if o.MeanBusy < 0.9 || o.MeanBusy > 1.1 || o.MeanCeiling < 9.9 || o.MeanCeiling > 10.1 {
		t.Errorf("busy %.2f of %.2f", o.MeanBusy, o.MeanCeiling)
	}
	if o.WorkerStarts != 1 || o.ReviewStarts != 1 || o.Lanes != 2 || o.Rollbacks != 2 {
		t.Errorf("starts %d/%d lanes %d rollbacks %d", o.WorkerStarts, o.ReviewStarts, o.Lanes, o.Rollbacks)
	}
	if fmt.Sprint(o.Parks) != "[{ci 1} {desk review 1}]" {
		t.Errorf("parks %v", o.Parks)
	}
	if len(o.MergeFailures) != 1 || o.MergeFailures[0].N != 1 || o.MergeFailures[0].Name[:6] != "exit 3" {
		t.Errorf("merge failures %v", o.MergeFailures)
	}
	if len(o.Failures) != 1 || o.Failures[0].N != 2 || o.Failures[0].Name != "review: o/r#N already requested this head or review round" {
		t.Errorf("failures %v", o.Failures)
	}
}

// TestCompareFitsAndRanks: the fitted model takes the log's values, and
// the what-ifs are ranked by their gain with the largest first.
func TestCompareFitsAndRanks(t *testing.T) {
	m := Default()
	m.Workers = 10
	o := Observed{
		Since: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC), Days: 7,
		Stages: []ObservedStage{
			{Stage: StageMerging, Residence: Quantiles{Count: 20, Median: 40 * time.Minute}},
			{Stage: StageReleasing, Residence: Quantiles{Count: 20, Median: 9 * time.Hour}},
			{Stage: StageCI, Residence: Quantiles{Count: 2, Median: 3 * time.Hour}},
		},
		Started: 200, Released: 100, Dropped: 50, DropShare: 1.0 / 3, StartedPerDay: 200.0 / 7, ReleasedPerDay: 100.0 / 7,
		LoopUptime: 0.5, LoopDown: 84 * time.Hour, Outages: []Outage{{From: o0(0), To: o0(84)}},
		MeanBusy: 2, MeanCeiling: 10, Utilization: 0.2, Lanes: 2,
	}
	c := Compare(m, o)
	f := c.Fitted
	if f.Stages[StageMerging].Median != 40*time.Minute || f.Stages[StageReleasing].Median != 9*time.Hour {
		t.Fatalf("fit took no stage times: %+v", f.Stages)
	}
	if f.Stages[StageCI].Median != m.Stages[StageCI].Median {
		t.Fatalf("fit took a stage of two samples")
	}
	if f.LoopUptime != 0.5 || f.Dropped < 0.33 || f.Dropped > 0.34 || f.Lanes != 2 || f.Backlog != 0 || f.ArrivalsPerDay != 200.0/7 {
		t.Fatalf("fit: %+v", f)
	}
	if len(c.WhatIfs) < 4 {
		t.Fatalf("what-ifs: %+v", c.WhatIfs)
	}
	for i := 1; i < len(c.WhatIfs); i++ {
		if c.WhatIfs[i].Gain > c.WhatIfs[i-1].Gain {
			t.Fatalf("what-ifs not ranked: %+v", c.WhatIfs)
		}
	}
	if len(c.Rows) != len(Stages) || c.Rows[5].Stage != StageMerging || c.Rows[5].Gap != 40*time.Minute-c.Rows[5].Expected.Median {
		t.Fatalf("rows: %+v", c.Rows)
	}
	if len(c.Findings) < 3 {
		t.Fatalf("findings: %v", c.Findings)
	}
}

func o0(h int) time.Time {
	return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(h) * time.Hour)
}

func TestFailureClassAndParkReason(t *testing.T) {
	for in, want := range map[string]string{
		`[intake o/r#3] {"message":"beekeeper board move: exit 1: beekeeper: gh api graphql -f: exit status 1: gh: Field 'login' doesn't exist","count":4}`: "intake: beekeeper board move: exit N: gh api graphql -f: exit status N: gh: Field 'login' doesn't exist",
		`[go o/r#3@0123456789abcdef] {"message":"no assigned independent review round for this execution and head"}`:                                        "go: no assigned independent review round for this execution and head",
		`plain text 12`: "loop: plain text N",
	} {
		if got := failureClass(in); got != want {
			t.Errorf("failureClass(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"w: parked on o/r#1: desk review":                      "desk review",
		"w: parked: Timer 88: natural SSH failure":             "timer",
		"w: parked: CI for https://x/pull/1 head abc":          "ci",
		"w: parked on o/r#1: awaiting loop merge":              "merge",
		"w: parked: Waiting until the owner is home; note 371": "person",
		"w: parked: Build capacity wait; timer 369 retries":    "timer",
		"w: parked on o/r#1: something nobody classified":      "other",
	} {
		if got := parkReason(in); got != want {
			t.Errorf("parkReason(%q) = %q, want %q", in, got, want)
		}
	}
}

// Refs of the forecast and check tests.
const (
	refTwo       = "o/r#2"
	refThree     = "o/r#3"
	refFour      = "o/r#4"
	refFive      = "o/r#5"
	refTwenty    = "o/r#20"
	refTwentyOne = "o/r#21"
)

// TestForecasting: a snapshot with the loop running runs its items forward
// through the model, names what will not move and starts the free board
// items as the slots allow.
func TestForecasting(t *testing.T) {
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	s := Snapshot{
		At: now, LoopRunning: true, Room: 3, Busy: 2, Parked: 1, Idle: 1, BoardRead: true,
		Reviewers: []string{"Review r 9 r1"},
		Items: []Item{
			{Ref: testRef, Step: StageImplementing, Since: now.Add(-20 * time.Minute), Agent: "Agent one", AgentState: AgentBusy},
			{Ref: refTwo, Step: StageImplementing, Since: now.Add(-7 * time.Hour), Agent: "Agent two", AgentState: AgentParked, Waits: "note 5"},
			{Ref: refThree, Pulls: []string{"o/r#13"}, Step: StageReviewQueued, Since: now.Add(-2 * time.Hour)},
			{Ref: refFour, Pulls: []string{"o/r#14"}, Step: StageMerging, Since: now.Add(-10 * time.Minute)},
			{Ref: refFive, Pulls: []string{"o/r#15"}, Step: StageReleasing, Since: now.Add(-5 * time.Minute)},
			{Ref: "o/r#9", Pulls: []string{"o/r#19"}, Step: StageReviewing, Since: now.Add(-5 * time.Minute), Agent: "Review r 9 r1", AgentState: AgentBusy},
		},
		Board:          []BoardItem{{Ref: refTwenty, Status: "Up Next"}, {Ref: "o/s#21", Status: "Backlog"}, {Ref: "o/r#22", Skip: "blocked"}, {Ref: testRef, Status: "In Progress"}},
		ReviewRequests: []state.ReviewRequest{{Ref: "o/r#13", Head: "abc", Round: 1}},
		FailedMerges:   []FailedMerge{{Ref: "o/r#14", At: now.Add(-30 * time.Minute), Exit: 5, Reason: "refused"}},
		ParkedAgents:   []ParkedAgent{{Name: "Agent two", On: refTwo, Waits: "note 5"}, {Name: "Agent six", Waits: "timer 88"}},
		Timers:         []int{3},
		UnmergedLimit:  5,
	}
	f := Forecasting(Default(), s, 48*time.Hour)
	wantProblem := func(sev, what string, refs ...string) {
		t.Helper()
		for _, p := range f.Problems {
			if p.Severity == sev && strings.Contains(p.What, what) {
				for _, r := range refs {
					if !slices.Contains(p.Refs, r) {
						t.Errorf("problem %q lacks %s: %v", what, r, p.Refs)
					}
				}
				return
			}
		}
		t.Errorf("no %s problem %q in %+v", sev, what, f.Problems)
	}
	wantProblem(SeverityStop, "a review is recorded as requested", refThree)
	wantProblem(SeveritySlow, "merge of o/r#14 failed", refFour)
	wantProblem(SeveritySlow, "1 item in implementing for over 6h00m with its worker parked", refTwo)
	wantProblem(SeveritySlow, "1 item in review-queued for over 1h00m", refThree)
	wantProblem(SeverityStop, "parked on a timer that no longer exists", "Agent six")
	wantProblem(SeveritySlow, "1 idle agent while 2 free board items wait")
	item := func(ref string) ItemForecast {
		t.Helper()
		for _, it := range f.Items {
			if it.Ref == ref {
				return it
			}
		}
		t.Fatalf("no forecast of %s", ref)
		return ItemForecast{}
	}
	if it := item(refTwo); !strings.Contains(it.Stuck, "parked") || !strings.Contains(it.Stuck, "note 5") || len(it.Timeline) != 0 {
		t.Errorf("parked worker: %+v", it)
	}
	if it := item(refThree); !strings.Contains(it.Stuck, "requested") {
		t.Errorf("stale review request: %+v", it)
	}
	one := item(testRef)
	var steps []string
	for _, tr := range one.Timeline {
		steps = append(steps, tr.Step)
	}
	for _, want := range []string{StageCI, StageReviewQueued, StageReviewing, StageMerging, StageReleasing, state.DeliveryDone} {
		if !slices.Contains(steps, want) {
			t.Errorf("o/r#1 never enters %s: %v", want, steps)
		}
	}
	if one.ReleaseAt.IsZero() || one.ReleaseAt.After(now.Add(48*time.Hour)) || one.Timeline[0].At.Before(now) {
		t.Errorf("o/r#1 release %s, first step %s", one.ReleaseAt, one.Timeline[0].At)
	}
	for _, ref := range []string{refFour, refFive, "o/r#9"} {
		if it := item(ref); it.ReleaseAt.IsZero() || it.Stuck != "" {
			t.Errorf("%s: %+v", ref, it)
		}
	}
	// The item in delivery that the board also lists is no start.
	if len(f.Starts) != 2 || f.Starts[0].Ref != refTwenty || f.Starts[1].Ref != "o/s#21" || f.Starts[0].At.After(f.Starts[1].At) {
		t.Fatalf("starts: %+v", f.Starts)
	}
	if item(refTwenty).Step != StepBoard {
		t.Errorf("a started board item is not a board item: %+v", item(refTwenty))
	}
	if len(f.Totals) != 4 {
		t.Fatalf("totals: %+v", f.Totals)
	}
	for i := 1; i < len(f.Totals); i++ {
		a, b := f.Totals[i-1], f.Totals[i]
		if b.After <= a.After || b.Started < a.Started || b.Released < a.Released {
			t.Errorf("totals not monotone: %+v", f.Totals)
		}
	}
	if last := f.Totals[len(f.Totals)-1]; last.Started != 2 || last.Released < 4 {
		t.Errorf("after the horizon: %+v", last)
	}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var back Forecast
	if err := json.Unmarshal(raw, &back); err != nil || len(back.Items) != len(f.Items) || back.Items[0].Stuck != f.Items[0].Stuck {
		t.Fatalf("round trip: %v", err)
	}

	// The loop down: nothing moves.
	down := s
	down.LoopRunning = false
	f = Forecasting(Default(), down, 24*time.Hour)
	if len(f.Problems) == 0 || f.Problems[0].Severity != SeverityStop || !strings.Contains(f.Problems[0].What, "loop is not running") {
		t.Fatalf("loop down: %+v", f.Problems)
	}
	for _, it := range f.Items {
		if it.Stuck == "" || len(it.Timeline) > 0 {
			t.Errorf("loop down, %s moves: %+v", it.Ref, it)
		}
	}
	if len(f.Starts) != 0 || len(f.Totals) != 3 || f.Totals[2].Released != 0 {
		t.Errorf("loop down: starts %v totals %v", f.Starts, f.Totals)
	}

	// No room: the items in flight move, the board waits.
	full := s
	full.StartBlocked = true
	full.Room, full.Blocks = 0, []string{"12 busy at capacity.ceiling 12"}
	f = Forecasting(Default(), full, 24*time.Hour)
	wantProblem(SeverityStop, "no start while 2 free board items wait", refTwenty, "o/s#21")
	if len(f.Starts) != 0 {
		t.Errorf("no room, starts: %+v", f.Starts)
	}
	if it := item(testRef); it.ReleaseAt.IsZero() {
		t.Errorf("no room stops an item in flight: %+v", it)
	}
}

// TestCheck: the forecast against the desk later, item by item.
func TestCheck(t *testing.T) {
	at := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	now := at.Add(3 * time.Hour)
	h := func(d time.Duration) time.Time { return at.Add(d) }
	f := Forecast{At: at, Horizon: 24 * time.Hour,
		Items: []ItemForecast{
			// on track: forecast in reviewing by now, there.
			{Ref: testRef, Step: StageImplementing, Timeline: []Transition{{StageCI, h(time.Hour)}, {StageReviewQueued, h(90 * time.Minute)}, {StageReviewing, h(2 * time.Hour)}, {StageMerging, h(5 * time.Hour)}}},
			// ahead: forecast in ci, already merging.
			{Ref: refTwo, Step: StageImplementing, Timeline: []Transition{{StageCI, h(2 * time.Hour)}, {StageReviewQueued, h(4 * time.Hour)}}},
			// behind: forecast in ci since +1h, still implementing.
			{Ref: refThree, Step: StageImplementing, Timeline: []Transition{{StageCI, h(time.Hour)}, {StageReviewQueued, h(4 * time.Hour)}}},
			// stuck as forecast.
			{Ref: refFour, Step: StageImplementing, Stuck: "its worker is parked"},
			// done as forecast.
			{Ref: refFive, Step: StageReleasing, Timeline: []Transition{{state.DeliveryDone, h(time.Hour)}}, ReleaseAt: h(time.Hour)},
			// gone: forecast releasing, no delivery left.
			{Ref: "o/r#6", Step: StageMerging, Timeline: []Transition{{StageReleasing, h(time.Hour)}, {state.DeliveryDone, h(6 * time.Hour)}}, ReleaseAt: h(6 * time.Hour)},
			// a board item forecast to start at +1h that did not.
			{Ref: refTwenty, Step: StepBoard, Timeline: []Transition{{StageImplementing, h(time.Hour)}, {StageCI, h(5 * time.Hour)}}},
			// a board item forecast to start at +5h, started early.
			{Ref: refTwentyOne, Step: StepBoard, Timeline: []Transition{{StageImplementing, h(5 * time.Hour)}}},
		},
		Starts:   []StartForecast{{Ref: refTwenty, At: h(time.Hour)}, {Ref: refTwentyOne, At: h(5 * time.Hour)}},
		Problems: []Problem{{Severity: SeverityStop, What: "stale", Refs: []string{refFour}}, {Severity: SeveritySlow, What: "slow", Refs: []string{refTwo, refThree}}, {Severity: SeverityStop, What: "loop"}},
	}
	items := []Item{
		{Ref: testRef, Step: StageReviewing},
		{Ref: refTwo, Step: StageMerging},
		{Ref: refThree, Step: StageImplementing, Agent: "Agent three", AgentState: AgentParked, Waits: "note 7"},
		{Ref: refFour, Step: StageImplementing, Agent: "Agent four", AgentState: AgentParked},
		{Ref: refTwentyOne, Step: StageImplementing},
	}
	c := Check(f, now, items, []string{refFive}, []string{refTwentyOne})
	want := map[string]string{testRef: VerdictOnTrack, refTwo: VerdictAhead, refThree: VerdictBehind, refFour: VerdictStuck, refFive: VerdictOnTrack, "o/r#6": VerdictGone, refTwenty: VerdictBehind, refTwentyOne: VerdictAhead}
	got := map[string]CheckedItem{}
	for _, it := range c.Items {
		got[it.Ref] = it
	}
	for ref, v := range want {
		if got[ref].Verdict != v {
			t.Errorf("%s: %+v, want %s", ref, got[ref], v)
		}
	}
	if got[refThree].Behind != 2*time.Hour || got[refThree].Waits != "note 7" || got[refTwenty].Behind != 2*time.Hour || got[refTwenty].Actual != StepBoard {
		t.Errorf("behind: %+v %+v", got[refThree], got[refTwenty])
	}
	if got[refFive].Expected != state.DeliveryDone || got[refFive].Actual != state.DeliveryDone {
		t.Errorf("done: %+v", got[refFive])
	}
	if c.Items[0].Verdict != VerdictBehind || c.Items[1].Verdict != VerdictBehind {
		t.Errorf("behind items first: %+v", c.Items)
	}
	if c.Forecast != (Tally{Started: 1, Released: 1}) || c.Actual != (Tally{Started: 1, Released: 1}) {
		t.Errorf("tallies: %+v %+v", c.Forecast, c.Actual)
	}
	if len(c.Problems) != 3 || c.Problems[0].Outcome != ProblemStanding || c.Problems[1].Outcome != ProblemStanding || c.Problems[1].Moved[0] != refTwo || c.Problems[1].Still[0] != refThree || c.Problems[2].Outcome != ProblemUnchecked {
		t.Errorf("problems: %+v", c.Problems)
	}
	if s := c.Summary(); !strings.HasPrefix(s, "2 of 8 items where the forecast said (2 ahead, 2 behind, 1 stuck as forecast, 1 gone)") {
		t.Errorf("summary: %s", s)
	}
}

// A pool that is full now makes room during the forecast. It is not a
// standing capacity refusal. A held merge cannot release through the hold.
func TestForecastOccupancyAndMergeHold(t *testing.T) {
	at := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	s := Snapshot{At: at, LoopRunning: true, Busy: 1, Room: 0,
		Blocks:      []string{"1 busy at the ceiling of 1"},
		Items:       []Item{{Ref: testRef, Step: StageImplementing, Since: at, AgentState: AgentBusy}},
		Board:       []BoardItem{{Ref: refTwo}},
		MergeBlocks: map[string]string{testRef: "merge held: o/r"},
	}
	m := Default()
	m.RedCI, m.ChangesRequested = 0, 0
	f := Forecasting(m, s, 18*time.Hour)
	if len(f.Starts) != 1 || !f.Starts[0].At.After(at) {
		t.Fatalf("full pool never admits waiting board item: %+v", f.Starts)
	}
	for _, it := range f.Items {
		if it.Ref == testRef && (!it.ReleaseAt.IsZero() || it.Stuck == "") {
			t.Fatalf("held item released: %+v", it)
		}
	}
	for i := 1; i < len(f.Totals); i++ {
		if f.Totals[i].After <= f.Totals[i-1].After {
			t.Fatalf("totals out of order: %+v", f.Totals)
		}
	}
}
