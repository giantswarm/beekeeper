package cmd

import (
	"encoding/json"
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
