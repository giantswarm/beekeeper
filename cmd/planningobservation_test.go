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

const (
	planningPull  = "o/r#11"
	planningRepoA = "o/a"
	planningRepoB = "o/b"
)

func TestDeliveryPlanningBoundsReleaseObservations(t *testing.T) {
	a, _ := supervisorApp(t)
	now := a.timeNow()
	seed(t, a, func(st *state.State) {
		st.Deliveries = []state.Delivery{{Ref: reserveIssue, Pulls: []string{planningPull}, Step: state.DeliveryReleasing, Started: now.Add(-time.Hour), Since: now.Add(-time.Minute)}}
	})
	old := readDeliveryRelease
	t.Cleanup(func() { readDeliveryRelease = old })
	called := false
	readDeliveryRelease = func(ctx context.Context, _ superloop.Pull) (string, string, error) {
		called = true
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > loopPlanningObservationTimeout {
			t.Fatal("release observation has no planning deadline")
		}
		return "", "", context.DeadlineExceeded
	}
	l := &supervisorLoop{a: a, said: map[string]bool{}}
	in := superloop.Input{Now: now, Pulls: map[string]superloop.Pull{planningPull: {Ref: planningPull, State: superloop.Merged, MergeCommit: loopHead}}}
	if err := l.observeDeliveries(context.Background(), in, nil); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("release not observed")
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Deliveries) != 1 || st.Deliveries[0].Step != state.DeliveryReleasing {
		t.Fatalf("unread release settled delivery: %+v", st.Deliveries)
	}
}

func TestBacklogPlanningBoundsAdmissionAndRotates(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	oldBudget := readLoopGraphQLBudget
	t.Cleanup(func() { readLoopGraphQLBudget = oldBudget })
	readLoopGraphQLBudget = func(a *app, ctx context.Context) (github.Budget, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > loopPlanningObservationTimeout {
			t.Fatal("shared admission has no planning deadline")
		}
		return oldBudget(a, ctx)
	}
	var order []string
	old := readRepoPullRevision
	t.Cleanup(func() { readRepoPullRevision = old })
	readRepoPullRevision = func(ctx context.Context, repo, _ string) (string, bool, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > loopPlanningObservationTimeout {
			t.Fatal("inventory observation has no planning deadline")
		}
		order = append(order, repo)
		return "", false, errors.New("unread")
	}
	in := superloop.Input{Pulls: map[string]superloop.Pull{"o/a#1": {State: superloop.Open}, "o/b#1": {State: superloop.Open}}}
	l.unmergedCursor = planningRepoA
	if err := l.checkUnmergedBacklogs(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != planningRepoB || order[1] != planningRepoA {
		t.Fatalf("resume order: %v", order)
	}
	// A shutdown cancels admission before any further repository is inspected.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.checkUnmergedBacklogs(ctx, in); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 {
		t.Fatalf("shutdown issued inventory reads: %v", order)
	}
}
