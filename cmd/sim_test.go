package cmd

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/sim"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	simTestRef     = "o/r#1"
	simStageFlag   = "--stage"
	simRunVerb     = "run"
	simObserveVerb = "observe"
)

// simArgs prefixes a sim verb to its arguments.
func simArgs(verb string, args ...string) []string { return append([]string{verb}, args...) }

// TestSimCommands: model, run, observe and compare print their figures from
// the configuration and the event log alone, and --json returns them.
func TestSimCommands(t *testing.T) {
	a, out := supervisorApp(t)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	a.now = now
	a.cfg.Capacity.Ceiling = 7
	a.cfg.Capacity.ReviewHeadroom = 2
	t.Setenv("PATH", t.TempDir())

	c := a.simCmd()
	c.SetArgs(simArgs("model"))
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "7 busy agents, reviewers up to 2 more") || !strings.Contains(out.String(), "the gate's run") {
		t.Fatalf("model: %s", out)
	}

	out.Reset()
	c = a.simCmd()
	c.SetArgs(simArgs(simRunVerb, "--days", "2", "--workers", "4", "--lanes", "1", simStageFlag, "merging=45m", "--seed", "3"))
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "released a day") || !strings.Contains(out.String(), "merging") {
		t.Fatalf("run: %s", out)
	}
	for _, bad := range [][]string{{simStageFlag, "nothing=1m"}, {simStageFlag, "merging=soon"}, {"--uptime", "0"}, {"--days", "0"}} {
		c = a.simCmd()
		c.SetArgs(simArgs(simRunVerb, bad...))
		if err := c.Execute(); err == nil || Code(err) != ExitUsage {
			t.Fatalf("%v: %v", bad, err)
		}
	}

	// The log: one delivery through every step, the loop up for the first
	// of two days, two busy of seven.
	since := now.Add(-2 * 24 * time.Hour)
	t0 := since.Add(time.Hour)
	var events []state.Event
	stepAt := func(at time.Time, step string) {
		raw, _ := json.Marshal(state.Delivery{Ref: simTestRef, Pulls: []string{}, Step: step, Since: at, Started: t0})
		events = append(events, state.Event{At: at, By: loopParty, Verb: state.DeliveryStepVerb, Subject: simTestRef, Detail: string(raw)})
	}
	at := t0
	for _, s := range []struct {
		step string
		d    time.Duration
	}{{state.DeliveryImplementing, 20 * time.Minute}, {state.DeliveryCI, 15 * time.Minute}, {state.DeliveryReviewQueued, 3 * time.Hour}, {state.DeliveryReviewing, 10 * time.Minute}, {state.DeliveryMerging, 50 * time.Minute}, {state.DeliveryReleasing, 6 * time.Hour}} {
		stepAt(at, s.step)
		at = at.Add(s.d)
	}
	stepAt(at, state.DeliveryDone)
	for at := since; at.Before(since.Add(24 * time.Hour)); at = at.Add(10 * time.Minute) {
		events = append(events, state.Event{At: at, By: loopParty, Verb: "supervisor.claim", Detail: "[go o/r#1@abc] w"})
	}
	for at := since; at.Before(now); at = at.Add(30 * time.Minute) {
		events = append(events, state.Event{At: at, By: watchParty, Verb: "watch.usage", Detail: "USAGE: codex: 2 busy of ceiling 7, usage 7d 30%"})
	}
	events = append(events,
		state.Event{At: t0, By: loopParty, Verb: "supervisor.failure", Detail: `[review o/r#2@abc] {"message":"o/r#2 already requested this head or review round","count":3}`},
		state.Event{At: t0, By: loopParty, Verb: "merge.failed", Detail: "o/r#3 exit 5, nothing merged: refused"},
		state.Event{At: t0, By: loopParty, Verb: "merging", Detail: "o/r#1 in lane o/r"},
	)
	if err := a.store.Update(func(*state.State) ([]state.Event, error) { return events, nil }); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	c = a.simCmd()
	c.SetArgs(simArgs(simObserveVerb, sinceFlag, "2d"))
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"1 deliveries seen, 1 started, 1 released, 0 dropped", "loop up 50%", "busy 2.0 of a ceiling of 7 (29%)", "review-queued  1  3h00m", "exit 5: refused", "review: o/r#N already requested"} {
		if !strings.Contains(got, want) {
			t.Errorf("observe lacks %q:\n%s", want, got)
		}
	}

	out.Reset()
	c = a.simCmd()
	c.SetArgs([]string{sinceFlag, "2d"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	got = out.String()
	for _, want := range []string{"STAGE", "review-queued", "3h00m", "1× review: o/r#N already requested", "releasing", "6h00m", "loop up", "50%", "busy agents", "One change at a time", "the loop always up", "every free slot filled from the board"} {
		if !strings.Contains(got, want) {
			t.Errorf("compare lacks %q:\n%s", want, got)
		}
	}

	out.Reset()
	a.json = true
	c = a.simCmd()
	c.SetArgs([]string{"compare", sinceFlag, "2d"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	var cmp sim.Comparison
	if err := json.Unmarshal(out.Bytes(), &cmp); err != nil {
		t.Fatal(err)
	}
	if cmp.Model.Workers != 7 || cmp.Fitted.LoopUptime < 0.49 || cmp.Fitted.LoopUptime > 0.51 || len(cmp.WhatIfs) == 0 || cmp.WhatIfs[0].Gain < cmp.WhatIfs[len(cmp.WhatIfs)-1].Gain {
		t.Fatalf("compare json: workers %d uptime %.2f what-ifs %+v", cmp.Model.Workers, cmp.Fitted.LoopUptime, cmp.WhatIfs)
	}
	for _, bad := range [][]string{{sinceFlag, "2026-11-02", "--until", "2026-11-01"}, {sinceFlag, "yesterday-ish"}} {
		c = a.simCmd()
		c.SetArgs(simArgs(simObserveVerb, bad...))
		if err := c.Execute(); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
}

// TestSimForecastAndCheck: forecast reads the deliveries, the roster and
// the capacity verdict in process, names the stuck items, saves the
// forecast, and check compares it with the desk.
func TestSimForecastAndCheck(t *testing.T) {
	a, out := supervisorApp(t)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	a.now = now
	a.cfg.Capacity.Ceiling = 7
	a.kindClusters = func() ([]string, error) { return nil, nil }
	t.Setenv("PATH", t.TempDir())
	if err := a.store.WriteFile(loopPIDFile, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	const stuckRef, staleRef, stalePull = "o/r#2", "o/r#3", "o/r#13"
	seed(t, a, func(st *state.State) {
		st.Deliveries = []state.Delivery{
			{Ref: simTestRef, Pulls: []string{}, Step: state.DeliveryImplementing, Since: now.Add(-20 * time.Minute), Started: now.Add(-20 * time.Minute), Agent: agentOne},
			{Ref: stuckRef, Pulls: []string{}, Step: state.DeliveryImplementing, Since: now.Add(-7 * time.Hour), Started: now.Add(-7 * time.Hour), Agent: agentTwo, Waits: "note 5"},
			{Ref: staleRef, Pulls: []string{stalePull}, Step: state.DeliveryReviewQueued, Since: now.Add(-2 * time.Hour), Started: now.Add(-3 * time.Hour)},
			{Ref: "o/r#71", Pulls: []string{"o/r#25"}, Step: state.DeliveryDone, Since: now.Add(-time.Hour), Started: now.Add(-5 * time.Hour)},
		}
		st.Agents = []state.Agent{
			{Party: state.Party{Name: agentOne, Session: "sim-one"}, Task: "work on " + simTestRef, AssignedAt: now.Add(-20 * time.Minute)},
			{Party: state.Party{Name: agentTwo, Session: "sim-two"}, Task: "work on " + stuckRef, AssignedAt: now.Add(-7 * time.Hour), Park: &state.Park{On: stuckRef, Waits: "note 5", At: now.Add(-6 * time.Hour)}},
		}
		st.ReviewRequests = []state.ReviewRequest{{Ref: stalePull, Head: "head-a", Round: 1}}
	})

	c := a.simCmd()
	c.SetArgs(simArgs(simCheckVerb))
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "no forecast saved") {
		t.Fatalf("check without a forecast: %v", err)
	}
	c = a.simCmd()
	c.SetArgs(simArgs("forecast", "--no-board", "--save=false"))
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"Forecast from " + clock(now, now) + " over 24h00m: loop running, 1 busy of 7 slots (room 6), 1 parked, 0 idle; board not read", "PROBLEMS", "stop  a review is recorded as requested", staleRef, "slow  1 item in implementing for over 6h00m with its worker parked", "EXPECTED", "by +6h00m", "ITEMS", "implementing 7h00m", "stuck: its worker is parked: note 5", "implementing 20m", "  ci "} {
		if !strings.Contains(got, want) {
			t.Errorf("forecast lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "o/r#71") {
		t.Errorf("a done delivery is forecast:\n%s", got)
	}
	c = a.simCmd()
	c.SetArgs(simArgs(simCheckVerb))
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "no forecast saved") {
		t.Fatalf("check after --save=false: %v", err)
	}

	out.Reset()
	a.json = true
	c = a.simCmd()
	c.SetArgs(simArgs("forecast", "--no-board", "--horizon", "12h"))
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	var f sim.Forecast
	if err := json.Unmarshal(out.Bytes(), &f); err != nil {
		t.Fatal(err)
	}
	if !f.Snapshot.LoopRunning || f.Snapshot.Room != 6 || f.Snapshot.Busy != 1 || len(f.Snapshot.Items) != 3 || f.Horizon != 12*time.Hour || len(f.Totals) != 2 {
		t.Fatalf("forecast json: %+v", f.Snapshot)
	}
	var saved sim.Forecast
	if found, err := a.store.ReadFile(simForecastFile, &saved); err != nil || !found || !saved.At.Equal(f.At) {
		t.Fatalf("saved forecast: %v %v", found, err)
	}

	// Two hours on: the worked item is merged, the parked one still parked.
	a.now = now.Add(2 * time.Hour)
	seed(t, a, func(st *state.State) {
		st.Deliveries[0].Step, st.Deliveries[0].Since = state.DeliveryMerging, a.now.Add(-5*time.Minute)
		st.Deliveries = append(st.Deliveries, state.Delivery{Ref: "o/r#81", Pulls: []string{}, Step: state.DeliveryImplementing, Since: a.now.Add(-time.Hour), Started: a.now.Add(-time.Hour), Agent: agentC.Name})
	})
	out.Reset()
	c = a.simCmd()
	c.SetArgs(simArgs(simCheckVerb))
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	var checked sim.Checked
	if err := json.Unmarshal(out.Bytes(), &checked); err != nil {
		t.Fatal(err)
	}
	// The worked item was forecast released within the two hours: merging
	// is behind.
	if checked.Actual.Started != 1 || checked.Counts[sim.VerdictStuck] != 2 || checked.Counts[sim.VerdictBehind] != 1 || checked.Items[0].Ref != simTestRef || checked.Items[0].Actual != state.DeliveryMerging {
		t.Fatalf("check json: %+v %+v", checked.Counts, checked.Items)
	}

	out.Reset()
	a.json = false
	c = a.simCmd()
	c.SetArgs(simArgs(simCheckVerb))
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	got = out.String()
	for _, want := range []string{"The forecast from " + clock(now, now) + " against the desk at " + clock(a.now, a.now) + " (2h00m later)", simTestRef + "  behind ", "forecast done, still merging", agentOne + " (busy)", "by now: forecast 0 started, 1 released; real 1 started, 0 released", "0 of 3 items where the forecast said (1 behind, 2 stuck as forecast)", "PROBLEMS STANDING", "stop  a review is recorded as requested"} {
		if !strings.Contains(got, want) {
			t.Errorf("check lacks %q:\n%s", want, got)
		}
	}
}

func TestSimSnapshotMergeHolds(t *testing.T) {
	const (
		repo       = "o/r"
		heldPull   = "o/r#12"
		exceptPull = "o/r#13"
		secondRef  = "o/r#2"
		thirdRef   = "o/r#3"
	)
	a, _ := supervisorApp(t)
	at := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	st := &state.State{Holds: []state.Hold{{Target: repo, Reason: "proof", At: at, Except: exceptPull}}}
	s := sim.Snapshot{At: at, Items: []sim.Item{
		{Ref: simTestRef, Pulls: []string{heldPull}, Step: sim.StageMerging},
		{Ref: secondRef, Pulls: []string{exceptPull}, Step: sim.StageMerging},
		{Ref: thirdRef, Pulls: []string{"o/r#14"}, Step: sim.StageReleasing},
	}, Board: []sim.BoardItem{{Ref: exceptPull}}}
	a.simMergeBlocks(st, &s)
	if s.MergeBlocks[simTestRef] == "" || s.MergeBlocks[exceptPull] == "" {
		t.Fatalf("held merges not blocked: %v", s.MergeBlocks)
	}
	if s.MergeBlocks[secondRef] != "" || s.MergeBlocks[thirdRef] != "" {
		t.Fatalf("exception or merged release blocked: %v", s.MergeBlocks)
	}
}
