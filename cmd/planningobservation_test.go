package cmd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
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
	old := loop.ReadDeliveryRelease
	t.Cleanup(func() { loop.ReadDeliveryRelease = old })
	called := false
	loop.ReadDeliveryRelease = func(ctx context.Context, _ superloop.Pull) (string, string, error) {
		called = true
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > loop.LoopPlanningObservationTimeout {
			t.Fatal("release observation has no planning deadline")
		}
		return "", "", context.DeadlineExceeded
	}
	l := &loop.Loop{Host: a, Said: map[string]bool{}}
	in := superloop.Input{Now: now, Pulls: map[string]superloop.Pull{planningPull: {Ref: planningPull, State: superloop.Merged, MergeCommit: loopHead}}}
	if err := l.ObserveDeliveries(context.Background(), in, nil); err != nil {
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
		if !ok || time.Until(deadline) > loop.LoopPlanningObservationTimeout {
			t.Fatal("shared admission has no planning deadline")
		}
		return oldBudget(a, ctx)
	}
	var order []string
	old := loop.ReadRepoPullRevision
	t.Cleanup(func() { loop.ReadRepoPullRevision = old })
	loop.ReadRepoPullRevision = func(ctx context.Context, repo, _ string) (string, bool, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > loop.LoopPlanningObservationTimeout {
			t.Fatal("inventory observation has no planning deadline")
		}
		order = append(order, repo)
		return "", false, errors.New("unread")
	}
	in := superloop.Input{Pulls: map[string]superloop.Pull{"o/a#1": {State: superloop.Open}, "o/b#1": {State: superloop.Open}}}
	l.UnmergedCursor = planningRepoA
	if err := l.CheckUnmergedBacklogs(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != planningRepoB || order[1] != planningRepoA {
		t.Fatalf("resume order: %v", order)
	}
	// A shutdown cancels admission before any further repository is inspected.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.CheckUnmergedBacklogs(ctx, in); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 {
		t.Fatalf("shutdown issued inventory reads: %v", order)
	}
}

func TestDeliveryIssueReadsLeaveReleasePlanningTime(t *testing.T) {
	a, _ := supervisorApp(t)
	now := a.timeNow()
	seed(t, a, func(st *state.State) {
		st.StartDelivery(epicTen, "issue worker", "", now)
		st.Deliveries = append(st.Deliveries, state.Delivery{Ref: reserveIssue, Pulls: []string{planningPull}, Step: state.DeliveryReleasing, Started: now, Since: now})
	})
	oldRefs, oldRelease := refStates, loop.ReadDeliveryRelease
	t.Cleanup(func() { refStates, loop.ReadDeliveryRelease = oldRefs, oldRelease })
	var issueDeadline time.Time
	refStates = func(ctx context.Context, _ []github.PR) (map[github.PR]github.RefState, error) {
		var ok bool
		issueDeadline, ok = ctx.Deadline()
		if !ok {
			t.Fatal("issue observation has no deadline")
		}
		return nil, context.DeadlineExceeded
	}
	releaseRead := false
	loop.ReadDeliveryRelease = func(ctx context.Context, _ superloop.Pull) (string, string, error) {
		deadline, ok := ctx.Deadline()
		if !ok || issueDeadline.IsZero() || deadline.Sub(issueDeadline) < loop.LoopPlanningObservationTimeout/3 {
			t.Fatal("issue reads left no release observation allowance")
		}
		releaseRead = true
		return VerdictAvailable, deliveryAvailableTag, nil
	}
	l := &loop.Loop{Host: a, Said: map[string]bool{}}
	in := superloop.Input{Now: now, Pulls: map[string]superloop.Pull{planningPull: {Ref: planningPull, State: superloop.Merged, MergeCommit: loopHead}}}
	if err := l.ObserveDeliveries(t.Context(), in, nil); err != nil {
		t.Fatal(err)
	}
	if !releaseRead {
		t.Fatal("release observation starved behind issue read")
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range st.Deliveries {
		if d.Ref == epicTen {
			if d.Step != state.DeliveryImplementing {
				t.Fatalf("failed issue read changed its delivery: %+v", d)
			}
			return
		}
	}
	t.Fatal("failed issue read removed its delivery")
}
