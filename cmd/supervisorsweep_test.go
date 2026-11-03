package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// An open pull request of the desk with no saved review park, no review
// history and no worker reaches a pass only through the sweep's list. The
// loop logs since when it has had no owner, keeps that across restarts,
// starts its reviewer once it has waited superloop.OrphanWait, and never
// a second one for the same stretch.
func TestLoopSweepsAnOwnerlessPullWithoutReviewMetadata(t *testing.T) {
	r := newLoopRig(t)
	const orphan, orphanURL = "o/r#9", "https://github.com/o/r/pull/9"
	r.pulls[orphan] = superloop.Pull{Ref: orphan, URL: orphanURL, State: superloop.Open, Head: loopHead, Green: true, Author: loopDesk, Branch: "fix-9"}
	listed := readOpenPulls
	t.Cleanup(func() { readOpenPulls = listed })
	readOpenPulls = func(_ context.Context, authors func() ([]string, error), _ []string) ([]string, error) {
		if a, err := authors(); err != nil || !slices.Equal(a, []string{loopDesk}) {
			t.Errorf("authors = %v, %v", a, err)
		}
		return []string{orphan}, nil
	}
	stretches := func() []state.Event {
		return slices.DeleteFunc(r.logged(t, loopOwnerlessVerb), func(e state.Event) bool { return e.Subject != orphan })
	}
	r.pass(t)
	if c := r.called(agentsName, agentStartName); c != nil {
		t.Fatalf("started before the wait: %v\n%s", c, r.out)
	}
	if evs := stretches(); len(evs) != 1 || !strings.HasPrefix(evs[0].Detail, "["+orphan+"] "+ownerlessSince) {
		t.Fatalf("ownerless lines = %+v", evs)
	}
	// A restarted loop keeps the stretch: no second line, no start.
	r.pass(t)
	if evs := stretches(); len(evs) != 1 {
		t.Fatalf("a restart began a new stretch: %+v", evs)
	}
	if c := r.called(agentsName, agentStartName); c != nil {
		t.Fatalf("started before the wait: %v", c)
	}
	// The stretch began OrphanWait ago.
	began := time.Now().Add(-superloop.OrphanWait - time.Minute)
	if err := r.a.store.Record(about(event(loopParty, loopOwnerlessVerb, "[%s] %s%s", orphan, ownerlessSince, began.UTC().Format(time.RFC3339Nano)), orphan)); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	start := r.called(agentsName, agentStartName)
	if start == nil || start[2] != superloop.ReviewerName(orphan, 1) || !slices.Contains(start, "--review") {
		t.Fatalf("no reviewer started: %v\n%s", r.calls, r.out)
	}
	if evs := r.logged(t, "supervisor.review"); len(evs) != 1 || evs[0].Subject != orphan {
		t.Errorf("logged %+v", evs)
	}
	// Its reviewer is not on the rig's roster: the pull request still has
	// no live owner, and the loop's start gives it the wait again.
	r.pass(t)
	if c := r.called(agentsName, agentStartName); c != nil {
		t.Errorf("a second start in the same stretch: %v", c)
	}
	// An owner on the roster ends the stretch.
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: superloop.PullWorkerName(orphan)}, Task: "fix " + orphanURL})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	if evs := stretches(); len(evs) == 0 || evs[len(evs)-1].Detail != "["+orphan+"] "+ownerlessEnd {
		t.Errorf("an owner did not end the stretch: %+v", evs)
	}
}

// A pull request a live agent's name or park owns is not read for the
// sweep at all.
func TestSweepPullsSkipsTrackedAndOwnedPulls(t *testing.T) {
	r := newLoopRig(t)
	listed := readOpenPulls
	t.Cleanup(func() { readOpenPulls = listed })
	readOpenPulls = func(context.Context, func() ([]string, error), []string) ([]string, error) {
		return []string{"o/r#7", "o/r#8", "o/r#9", "o/r#10", "x/y#1"}, nil
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	in := superloop.Input{Desk: superloop.Desk{Orgs: []string{"o"}, Authors: []string{loopDesk}}, Workers: []superloop.Worker{
		{Name: "r 8"},
		{Name: "someone", ParkedOn: "o/r#9"},
		{Name: "gone", ParkedOn: "o/r#10", ReviewRequested: true, Saved: true},
	}}
	got := l.sweepPulls(context.Background(), in, []string{"o/r#7"})
	if want := []string{"o/r#10"}; !slices.Equal(got, want) {
		t.Errorf("sweep reads %v, want %v", got, want)
	}
}
