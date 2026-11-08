package sim

import (
	"encoding/json"
	"fmt"
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
		"w: parked on o/r#1: desk review":                   "desk review",
		"w: parked: Timer 88: natural SSH failure":          "timer",
		"w: parked: CI for https://x/pull/1 head abc":       "ci",
		"w: parked on o/r#1: awaiting loop merge":           "merge",
		"w: parked: Waiting until teemow is home; note 371": "person",
		"w: parked: Build capacity wait; timer 369 retries": "timer",
		"w: parked on o/r#1: something nobody classified":   "other",
	} {
		if got := parkReason(in); got != want {
			t.Errorf("parkReason(%q) = %q, want %q", in, got, want)
		}
	}
}
