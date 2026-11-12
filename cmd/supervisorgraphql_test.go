package cmd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func TestLoopGraphQLAllowancePersistsAndProtectsFloor(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = 2500
	healthyLoopBudget(t, r)
	remaining := 5000
	reset := r.a.now.Add(time.Hour)
	readLoopGraphQLBudget = func(*app, context.Context) (github.Budget, error) {
		remaining-- // the fresh probe itself costs a point
		return github.Budget{Remaining: 4900, GraphQL: &github.GraphQL{Remaining: remaining, Limit: 5000, Reset: reset}}, nil
	}
	runs, pullRuns := 0, 0
	start := r.a.now
	for attempt := range 200 {
		r.a.now = start.Add(time.Duration(attempt) * 18 * time.Second)
		if attempt%5 == 0 {
			remaining -= 3
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Budget = &state.Budget{
					Remaining: 4900, Limit: 5000, Reset: reset, At: r.a.now,
					GraphQL: &state.GraphQL{Remaining: remaining, Limit: 5000, Reset: reset, At: r.a.now},
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
		} // 120 watch probes across the hour
		// Every attempt uses a new app, as parent/child commands and a
		// restarted loop do. The allowance is shared through the store.
		a := *r.a
		err := a.loopGitHub(context.Background(), loop.LoopBoard, func() error { runs++; remaining -= 39; return nil })
		if err != nil && !errors.Is(err, loop.ErrLoopLater) {
			t.Fatal(err)
		}
		if attempt%5 == 0 {
			if err := a.loopGitHub(context.Background(), loop.LoopPullStep, func() error { pullRuns++; return nil }); err != nil {
				t.Fatalf("interleaved pull step held at attempt %d: %v", attempt, err)
			}
		}
	}
	if runs == 0 || pullRuns != 40 || 5000-remaining > loopGraphQLLimit || remaining < 2500 {
		t.Fatalf("runs=%d pullRuns=%d spent=%d remaining=%d", runs, pullRuns, 5000-remaining, remaining)
	}
	var spend loopGraphQLSpend
	if _, err := r.a.store.ReadFile(loopGraphQLFile, &spend); err != nil {
		t.Fatal(err)
	}
	if spend.Steps[loop.LoopBoard] <= loopGraphQLLimit-loopGraphQLProbeReserve-loop.LoopGraphQLReserve {
		t.Fatalf("board spend was wiped mid-window: %+v", spend)
	}
	before := runs
	remaining = 2550
	if err := r.a.loopGitHub(context.Background(), loop.LoopPullStep, func() error { runs++; return nil }); !errors.Is(err, loop.ErrLoopLater) || runs != before {
		t.Fatalf("admitted work without room above floor: %v", err)
	}
	// Only a new GitHub window releases the durable allowance.
	remaining = 5000
	reset = reset.Add(time.Hour)
	r.a.now = r.a.now.Add(time.Hour)
	if err := r.a.loopGitHub(context.Background(), loop.LoopPullStep, func() error { runs++; return nil }); err != nil || runs != before+1 {
		t.Fatalf("new window did not release allowance: %v", err)
	}
}

func TestLoopTimerChecksHoldGitHubButKeepLocalProbes(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = 2500
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = &state.Budget{Remaining: 4900, Reset: r.a.now.Add(time.Hour), At: r.a.now,
			GraphQL: &state.GraphQL{Remaining: 2400, Limit: 5000, Reset: r.a.now.Add(time.Hour), At: r.a.now}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	readLoopGraphQLBudget = func(*app, context.Context) (github.Budget, error) {
		t.Fatal("held allowance issued another budget probe")
		return github.Budget{}, nil
	}
	fakeRead(t, func(cmd string) ([]byte, error) {
		if cmd != probeHolds {
			t.Fatalf("GitHub timer ran under held allowance: %s", cmd)
		}
		return nil, nil
	})
	found := r.a.loopTimerChecks(context.Background(), []state.Timer{
		{ID: 1, Due: r.a.now.Add(-time.Minute), When: prMerged7},
		{ID: 2, Due: r.a.now.Add(-time.Minute), Probe: probeHolds},
	}, r.a.now, false)
	if _, ok := found[1]; ok {
		t.Fatal("GitHub condition was checked")
	}
	if _, ok := found[2]; !ok {
		t.Fatal("local probe was swallowed by GitHub hold")
	}
}

func TestLoopAllowanceHoldReleasesWhenLiveBudgetAboveFloor(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = 2500
	start := r.a.now
	liveReset := start.Add(34 * time.Minute)
	// Seed a held loop allowance with a blind 1-hour reset, as seen when a
	// previous pass held the allowance until the next hourly turn.
	if err := r.a.store.WriteFile(loopGraphQLFile, loopGraphQLSpend{
		Reset:     start.Add(time.Hour),
		HeldAt:    start.Add(-5 * time.Minute),
		Remaining: 2400,
		Spent:     loopGraphQLLimit,
		Steps:     map[string]int{loop.LoopBoard: loopGraphQLLimit},
		Accounts:  map[string]loopGraphQLAccount{"gh": {Reset: start.Add(time.Hour), Remaining: 2400}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = &state.Budget{
			Remaining: 3014, Limit: 5000, Reset: liveReset, At: start,
			GraphQL: &state.GraphQL{Remaining: 4334, Limit: 5000, Reset: liveReset, At: start},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	remaining := 4334
	readLoopGraphQLBudget = func(*app, context.Context) (github.Budget, error) {
		remaining--
		// Return two identities sharing a login and a 1s resetAt jitter on
		// the second call to verify neither doubles spend nor retains a
		// 500-point reservation.
		jitter := liveReset.Add(time.Duration(4334-remaining) * time.Second)
		profiles := map[string]github.Budget{
			"gh": {
				Identity: "gh", Login: loopDesk, Remaining: 3014, Limit: 5000, Reset: liveReset,
				GraphQL: &github.GraphQL{Identity: "gh", Login: loopDesk, Limit: 5000, Remaining: remaining, Reset: jitter},
			},
			github.IdentityApp: {
				Identity: github.IdentityApp, Login: loopDesk, Remaining: 3014, Limit: 5000, Reset: liveReset,
				GraphQL: &github.GraphQL{Identity: github.IdentityApp, Login: loopDesk, Limit: 5000, Remaining: remaining, Reset: jitter},
			},
		}
		return github.LowestBudget(profiles, r.a.now), nil
	}
	for _, step := range []string{loop.LoopPullStep, "park-refs", loop.LoopBoard} {
		ran := false
		if err := r.a.loopGitHub(context.Background(), step, func() error {
			ran = true
			remaining -= 2
			return nil
		}); err != nil || !ran {
			t.Fatalf("step %s held despite healthy live budget: ran=%t err=%v", step, ran, err)
		}
	}
	var spend loopGraphQLSpend
	if _, err := r.a.store.ReadFile(loopGraphQLFile, &spend); err != nil {
		t.Fatal(err)
	}
	if !sameGraphQLWindow(spend.Reset, liveReset) || spend.Reset.Equal(start.Add(time.Hour)) {
		t.Fatalf("allowance kept blind hourly reset %s instead of live window %s", spend.Reset, liveReset)
	}
	if len(spend.Accounts) != 1 || spend.Spent > 20 {
		t.Fatalf("shared login or jitter inflated spend: %+v", spend)
	}
}

func TestHeldLoopWithHealthyBudgetResumesReviewsAndMergeGosNextRound(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = 2500
	const reviewPR = "o/r#8"
	const reviewURL = "https://github.com/o/r/pull/8"
	const reviewer8 = "Review r 8 r1"
	r.pulls[reviewPR] = superloop.Pull{Ref: reviewPR, URL: reviewURL, State: superloop.Open, Head: loopHead, Green: true, Author: loopDesk}

	// Set up loopPR (o/r#7) with a completed review round so its next step
	// is the merge go, and reviewPR (o/r#8) freshly parked so its next step
	// is starting a reviewer.
	healthyLoopBudget(t, r)
	r.park(t, loopPR, r.a.now)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.Once = true // These checks inspect completed pass effects.
	if err := l.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if start := r.called(agentsName, agentStartName); start == nil || start[2] != loopReviewer {
		t.Fatalf("setup did not start reviewer for %s: %v", loopPR, r.calls)
	}
	r.verdict("R1", loopHead, superloop.Approved, "")
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{
			Party: state.Party{Name: loopSecondWorker},
			Task:  "fix " + reviewURL,
			Park:  &state.Park{On: reviewPR, At: r.a.now, Waits: deskReviewWait, ReviewHead: loopHead},
		})
		done := event(state.Party{Name: loopReviewer}, superloop.VerbDone, "%s done: %s", loopReviewer, loopPR)
		done.At = r.a.now.Add(10 * time.Second)
		return []state.Event{done}, nil
	}); err != nil {
		t.Fatal(err)
	}

	// Round 1: hold the loop both via a low budget reading and via a held
	// loop-graphql.json allowance file. Neither the review for o/r#8 nor
	// the merge go for o/r#7 may run, and no supervisor.failure may be logged.
	r.a.now = r.a.now.Add(30 * time.Second)
	if err := r.a.store.WriteFile(loopGraphQLFile, loopGraphQLSpend{
		Reset:     r.a.now.Add(time.Hour),
		HeldAt:    r.a.now,
		Remaining: 2400,
		Spent:     loopGraphQLLimit,
		Steps:     map[string]int{loop.LoopBoard: loopGraphQLLimit},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = &state.Budget{
			Remaining: 2400, Limit: 5000, Reset: r.a.now.Add(time.Hour), At: r.a.now,
			GraphQL: &state.GraphQL{Remaining: 2400, Limit: 5000, Reset: r.a.now.Add(time.Hour), At: r.a.now},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.calls = nil
	if err := l.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.called(agentsName, agentStartName) != nil || r.called(gateCmdName) != nil {
		t.Fatalf("held loop started review or gave merge go: %v", r.calls)
	}
	if failures := r.logged(t, loop.LoopFailureVerb); len(failures) != 0 {
		t.Fatalf("held loop logged failures: %+v", failures)
	}

	// Round 2 (next round, 30s later, still within the old 1-hour hold
	// window): publish a healthy live budget reading (REST 3014/5000,
	// GraphQL 4334/5000 with floor 2500). The loop must release its
	// allowance hold and both start the review for o/r#8 and give the merge
	// go for o/r#7 in this round.
	r.a.now = r.a.now.Add(30 * time.Second)
	liveReset := r.a.now.Add(36 * time.Minute)
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = &state.Budget{
			Remaining: 3014, Limit: 5000, Reset: liveReset, At: r.a.now,
			GraphQL: &state.GraphQL{Remaining: 4334, Limit: 5000, Reset: liveReset, At: r.a.now},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.calls = nil
	if err := l.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := r.called(agentsName, agentStartName)
	if start == nil || start[2] != reviewer8 || !slices.Contains(start, "--review") {
		t.Fatalf("next round did not start review for %s: calls=%v\n%s", reviewPR, r.calls, r.out)
	}
	mergeCall := r.called(gateCmdName)
	if mergeCall == nil || !slices.Contains(mergeCall, loopHead) {
		t.Fatalf("next round did not give merge go for %s: calls=%v\n%s", loopPR, r.calls, r.out)
	}
	if evs := r.logged(t, "supervisor.merge"); len(evs) != 1 || !strings.Contains(evs[0].Detail, "[go "+loopPR+"@"+loopHead+"]") {
		t.Fatalf("merge go not recorded: %+v", evs)
	}
	if failures := r.logged(t, loop.LoopFailureVerb); len(failures) != 0 {
		t.Fatalf("unexpected supervisor.failure events: %+v", failures)
	}
	var spend loopGraphQLSpend
	if _, err := r.a.store.ReadFile(loopGraphQLFile, &spend); err != nil {
		t.Fatal(err)
	}
	if !spend.Reset.Equal(liveReset) || !spend.HeldAt.IsZero() {
		t.Fatalf("allowance hold was not released to live reset %s: %+v", liveReset, spend)
	}
	r.answer = func(args []string) (string, int) {
		if slices.Contains(args, loop.LoopBoard) {
			return "Error: loop GitHub allowance held until 2026-11-02T22:30:00Z: later", 1
		}
		return "", 0
	}
	if _, err := l.Run(context.Background(), loop.LoopBoard, "o/r#9", "backlog", "--intake"); !errors.Is(err, loop.ErrLoopLater) {
		t.Fatalf("held child board command did not map to errLoopLater: %v", err)
	}
}

func TestLoopPullInvalidationIsSpecificToItsThread(t *testing.T) {
	now := time.Now()
	in := superloop.Input{Workers: []superloop.Worker{{ParkedOn: loopPR, ParkedAt: now}}, Events: []superloop.Event{
		{Agent: superloop.ReviewerName("other/repo#2", 1), At: now.Add(time.Minute)},
	}}
	if got := loop.LoopPullChanged(in, loopPR); !got.Equal(now) {
		t.Fatalf("unrelated PR invalidated cache: %s", got)
	}
	for round := 1; round <= 4; round++ {
		in.Taken = append(in.Taken, superloop.Taken{Ref: loopPR, Kind: superloop.KindReview})
		at := now.Add(time.Duration(round+1) * time.Minute)
		in.Events = append(in.Events, superloop.Event{Agent: superloop.ReviewerName(loopPR, round), At: at})
		if got := loop.LoopPullChanged(in, loopPR); !got.Equal(at) {
			t.Fatalf("reviewer round %d completion missed: %s", round, got)
		}
	}
}

func TestLoopRetainsReservationWhenSpendCannotBeRead(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = 2500
	healthyLoopBudget(t, r)
	probes := 0
	readLoopGraphQLBudget = func(*app, context.Context) (github.Budget, error) {
		probes++
		if probes > 1 {
			return github.Budget{}, errors.New("connection lost")
		}
		return github.Budget{Remaining: 4900, GraphQL: &github.GraphQL{Remaining: 4900, Limit: 5000, Reset: r.a.now.Add(time.Hour)}}, nil
	}
	if err := r.a.loopGitHub(context.Background(), "board", func() error { return nil }); !errors.Is(err, loop.ErrLoopLater) {
		t.Fatalf("unknown spend was not held: %v", err)
	}
	var spend loopGraphQLSpend
	if _, err := r.a.store.ReadFile(loopGraphQLFile, &spend); err != nil {
		t.Fatal(err)
	}
	if spend.Spent != loop.LoopGraphQLReserve || spend.Steps["board"] != loop.LoopGraphQLReserve {
		t.Fatalf("lost interrupted request's reservation: %+v", spend)
	}
}

func healthyLoopBudget(t *testing.T, r *loopRig) {
	t.Helper()
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = &state.Budget{Remaining: 4900, Reset: r.a.now.Add(time.Hour), At: r.a.now,
			GraphQL: &state.GraphQL{Remaining: 4900, Limit: 5000, Reset: r.a.now.Add(time.Hour), At: r.a.now}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLoopGraphQLFloorHoldsAllGitHubObservations(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = 2500
	r.park(t, loopPR, r.a.now)
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = &state.Budget{Remaining: 4900, Limit: 5000, At: r.a.now, Reset: r.a.now.Add(time.Hour),
			GraphQL: &state.GraphQL{Remaining: 2499, Limit: 5000, At: r.a.now, Reset: r.a.now.Add(time.Hour)}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	loop.ReadLoopPull = func(context.Context, string) (superloop.Pull, error) {
		t.Fatal("read PR under GraphQL floor")
		return superloop.Pull{}, nil
	}
	r.pass(t)
	if r.teamReads != 0 || r.called(loop.LoopBoard) != nil || r.called(agentsName, agentStartName) != nil {
		t.Fatalf("GitHub work under floor: teams=%d, commands=%v", r.teamReads, r.calls)
	}
}

func TestLoopGraphQLFloorKeepsWallClockTimers(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = 2500
	wakes := 0
	old := wakeOwner
	t.Cleanup(func() { wakeOwner = old })
	wakeOwner = func(*app, context.Context, state.Party, string, string, string) error { wakes++; return nil }
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Timers = []state.Timer{{ID: 1, Due: r.a.now.Add(-time.Minute), Wake: loopWorker, What: "local result ready"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	if wakes != 1 {
		t.Fatalf("GitHub hold swallowed a wall-clock timer: %d wakes", wakes)
	}
}

func TestLoopRequiresFreshGraphQLRecovery(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		g    *state.GraphQL
		held bool
	}{
		{kindUnknown, nil, true},
		{"at floor", &state.GraphQL{Remaining: 2500, At: now, Reset: now.Add(time.Hour)}, true},
		{kindStale, &state.GraphQL{Remaining: 4900, At: now.Add(-2 * time.Minute), Reset: now.Add(time.Hour)}, true},
		{"reset passed", &state.GraphQL{Remaining: 4900, At: now, Reset: now.Add(-time.Second)}, true},
		{"fresh quota recovery", &state.GraphQL{Remaining: 4900, At: now, Reset: now.Add(time.Hour)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &state.Budget{Remaining: 4900, At: now, Reset: now.Add(time.Hour), GraphQL: tc.g}
			if got := loop.LoopBudgetUnknownOrLow(b, 2500, now); got != tc.held {
				t.Fatalf("held=%t, want %t", got, tc.held)
			}
		})
	}
}
