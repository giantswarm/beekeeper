package cmd

import (
	"context"
	"fmt"
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
	listed := ReadOpenPulls
	t.Cleanup(func() { ReadOpenPulls = listed })
	ReadOpenPulls = func(_ context.Context, authors func() ([]string, error), _ []string) ([]string, error) {
		if a, err := authors(); err != nil || !slices.Equal(a, []string{loopDesk}) {
			t.Errorf("authors = %v, %v", a, err)
		}
		return []string{orphan}, nil
	}
	stretches := func() []state.Event {
		return slices.DeleteFunc(r.logged(t, LoopOwnerlessVerb), func(e state.Event) bool { return e.Subject != orphan })
	}
	r.pass(t)
	if c := r.called(agentsName, agentStartName); c != nil {
		t.Fatalf("started before the wait: %v\n%s", c, r.out)
	}
	if evs := stretches(); len(evs) != 1 || !strings.HasPrefix(evs[0].Detail, "["+orphan+"] "+OwnerlessSince) {
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
	if err := r.a.store.Record(about(event(LoopParty, LoopOwnerlessVerb, "[%s] %s%s", orphan, OwnerlessSince, began.UTC().Format(time.RFC3339Nano)), orphan)); err != nil {
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
	if evs := stretches(); len(evs) == 0 || evs[len(evs)-1].Detail != "["+orphan+"] "+OwnerlessEnd {
		t.Errorf("an owner did not end the stretch: %+v", evs)
	}
}

// sweepRef is pull request n of o/r.
func sweepRef(n int) string { return fmt.Sprintf("o/r#%d", n) }

// A pull request a live agent's name or park owns is not read for the
// sweep at all.
func TestSweepPullsSkipsTrackedAndOwnedPulls(t *testing.T) {
	r := newLoopRig(t)
	listed := ReadOpenPulls
	t.Cleanup(func() { ReadOpenPulls = listed })
	ReadOpenPulls = func(context.Context, func() ([]string, error), []string) ([]string, error) {
		return []string{loopPR, sweepRef(8), sweepRef(9), sweepRef(10), "x/y#1"}, nil
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	in := superloop.Input{Desk: superloop.Desk{Orgs: []string{"o"}, Authors: []string{loopDesk}}, Workers: []superloop.Worker{
		{Name: "r 8"},
		{Name: "someone", ParkedOn: sweepRef(9)},
		{Name: "left", ParkedOn: sweepRef(10), ReviewRequested: true, Saved: true},
	}}
	got := l.SweepPulls(context.Background(), in, []string{loopPR})
	if want := []string{sweepRef(10)}; !slices.Equal(got, want) {
		t.Errorf("sweep reads %v, want %v", got, want)
	}
}

// Delivery discovery survives an exhausted background allowance, and failed
// listings retry the next pass rather than postponing recovery for five minutes.
func TestSweepDiscoverySurvivesDrainAndRetriesFailedListing(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = 0
	if err := r.a.store.WriteFile(loopRESTFile, loopRESTSpend{Reset: r.a.timeNow().Add(time.Hour), Spent: loopRESTLimit}); err != nil {
		t.Fatal(err)
	}
	old := ReadOpenPulls
	t.Cleanup(func() { ReadOpenPulls = old })
	attempts := 0
	ReadOpenPulls = func(context.Context, func() ([]string, error), []string) ([]string, error) {
		attempts++
		if attempts == 1 {
			return nil, fmt.Errorf("temporarily unavailable")
		}
		return []string{loopPR}, nil
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	in := superloop.Input{Desk: superloop.Desk{Orgs: []string{"o"}, Authors: []string{loopDesk}}}
	l.SweepPulls(context.Background(), in, nil)
	if !l.OpenPullsAt.IsZero() {
		t.Fatal("failed listing marked fresh")
	}
	got := l.SweepPulls(context.Background(), in, nil)
	if attempts != 2 || !slices.Equal(got, []string{loopPR}) {
		t.Fatalf("discovery after drain: %v attempts %d", got, attempts)
	}
	l.SweepPulls(context.Background(), in, nil)
	if attempts != 2 {
		t.Fatal("successful discovery ignored listing interval")
	}
}

// Owners and repositories are searched in separate gh search prs calls so gh
// does not AND --owner with --repo, and large repository lists are chunked.
func TestSweepFlagBatchesSeparatesOwnersAndChunksRepos(t *testing.T) {
	sharedRepo := func(i int) string { return fmt.Sprintf("shared/r-%d", i) }
	var scopes []string
	scopes = append(scopes, "o")
	for i := 1; i <= SweepRepoBatch+2; i++ {
		scopes = append(scopes, sharedRepo(i))
	}
	batches := SweepFlagBatches(scopes)
	if len(batches) != 3 {
		t.Fatalf("batches = %d, want 3: %v", len(batches), batches)
	}
	if !slices.Equal(batches[0], []string{SweepOwnerFlag, "o"}) {
		t.Errorf("owner batch = %v", batches[0])
	}
	if len(batches[1]) != 2*SweepRepoBatch || batches[1][0] != LoopRepoFlag || batches[1][1] != sharedRepo(1) {
		t.Errorf("first repo batch = %v", batches[1])
	}
	if !slices.Equal(batches[2], []string{LoopRepoFlag, sharedRepo(SweepRepoBatch + 1), LoopRepoFlag, sharedRepo(SweepRepoBatch + 2)}) {
		t.Errorf("second repo batch = %v", batches[2])
	}
}
