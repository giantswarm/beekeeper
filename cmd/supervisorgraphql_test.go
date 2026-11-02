package cmd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func TestLoopGraphQLAllowancePersistsAndProtectsFloor(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = 2500
	remaining := 5000
	reset := r.a.now.Add(time.Hour)
	readLoopGraphQLBudget = func(*app, context.Context) (github.Budget, error) {
		remaining-- // the fresh probe itself costs a point
		return github.Budget{Remaining: 4900, GraphQL: &github.GraphQL{Remaining: remaining, Limit: 5000, Reset: reset}}, nil
	}
	runs := 0
	for range 200 {
		// Every attempt uses a new app, as parent/child commands and a
		// restarted loop do. The allowance is shared through the store.
		a := *r.a
		err := a.loopGitHub(context.Background(), "board", func() error { runs++; remaining -= 39; return nil })
		if err != nil && !errors.Is(err, errLoopLater) {
			t.Fatal(err)
		}
	}
	if runs == 0 || 5000-remaining > loopGraphQLLimit || remaining < 2500 {
		t.Fatalf("runs=%d spent=%d remaining=%d", runs, 5000-remaining, remaining)
	}
	before := runs
	remaining = 2550
	if err := r.a.loopGitHub(context.Background(), "pull", func() error { runs++; return nil }); !errors.Is(err, errLoopLater) || runs != before {
		t.Fatalf("admitted work without room above floor: %v", err)
	}
	// Only a new GitHub window releases the durable allowance.
	remaining = 5000
	reset = reset.Add(time.Hour)
	r.a.now = r.a.now.Add(time.Hour)
	if err := r.a.loopGitHub(context.Background(), "pull", func() error { runs++; return nil }); err != nil || runs != before+1 {
		t.Fatalf("new window did not release allowance: %v", err)
	}
}

func TestLoopPullInvalidationIsSpecificToItsThread(t *testing.T) {
	now := time.Now()
	in := superloop.Input{Workers: []superloop.Worker{{ParkedOn: loopPR, ParkedAt: now}}, Events: []superloop.Event{
		{Agent: superloop.ReviewerName("other/repo#2", 1), At: now.Add(time.Minute)},
	}}
	if got := loopPullChanged(in, loopPR); !got.Equal(now) {
		t.Fatalf("unrelated PR invalidated cache: %s", got)
	}
	in.Events = append(in.Events, superloop.Event{Agent: superloop.ReviewerName(loopPR, 1), At: now.Add(2 * time.Minute)})
	if got := loopPullChanged(in, loopPR); !got.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("reviewer completion missed: %s", got)
	}
}

func TestLoopRetainsReservationWhenSpendCannotBeRead(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = 2500
	probes := 0
	readLoopGraphQLBudget = func(*app, context.Context) (github.Budget, error) {
		probes++
		if probes > 1 {
			return github.Budget{}, errors.New("connection lost")
		}
		return github.Budget{Remaining: 4900, GraphQL: &github.GraphQL{Remaining: 4900, Limit: 5000, Reset: r.a.now.Add(time.Hour)}}, nil
	}
	if err := r.a.loopGitHub(context.Background(), "board", func() error { return nil }); !errors.Is(err, errLoopLater) {
		t.Fatalf("unknown spend was not held: %v", err)
	}
	var spend loopGraphQLSpend
	if _, err := r.a.store.ReadFile(loopGraphQLFile, &spend); err != nil {
		t.Fatal(err)
	}
	if spend.Spent != loopGraphQLReserve || spend.Steps["board"] != loopGraphQLReserve {
		t.Fatalf("lost interrupted request's reservation: %+v", spend)
	}
}

func TestLoopReusesPullReadsAndRefreshesOnLocalChanges(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	readLoopPull = func(context.Context, string) (superloop.Pull, error) {
		reads++
		return r.pull, nil
	}
	start := r.a.now
	changed := start.Add(-time.Minute)
	// Forty PRs over a full hour of 30-second passes use 480 reads, not
	// 4,800. No real GitHub calls or wall-clock waits are involved.
	for tick := 0; tick < 120; tick++ {
		r.a.now = start.Add(time.Duration(tick) * 30 * time.Second)
		for n := 0; n < 40; n++ {
			ref := string(rune('A' + n))
			if _, err := l.observePull(context.Background(), ref, changed); err != nil {
				t.Fatal(err)
			}
		}
	}
	if reads != 480 {
		t.Fatalf("%d PR reads in an hour, want 480", reads)
	}
	// A re-park, reviewer report or recorded answer refreshes immediately,
	// even though the five-minute interval has not elapsed.
	changed = r.a.now
	if _, err := l.observePull(context.Background(), "A", changed); err != nil {
		t.Fatal(err)
	}
	if reads != 481 {
		t.Fatalf("local change did not invalidate observation: %d", reads)
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
	readLoopPull = func(context.Context, string) (superloop.Pull, error) {
		t.Fatal("read PR under GraphQL floor")
		return superloop.Pull{}, nil
	}
	r.pass(t)
	if r.teamReads != 0 || r.called(loopBoard) != nil || r.called(agentsName, agentStartName) != nil {
		t.Fatalf("GitHub work under floor: teams=%d, commands=%v", r.teamReads, r.calls)
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
		{"recovered", &state.GraphQL{Remaining: 4900, At: now, Reset: now.Add(time.Hour)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &state.Budget{Remaining: 4900, At: now, Reset: now.Add(time.Hour), GraphQL: tc.g}
			if got := loopBudgetUnknownOrLow(b, 2500, now); got != tc.held {
				t.Fatalf("held=%t, want %t", got, tc.held)
			}
		})
	}
}
