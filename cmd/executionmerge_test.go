package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func TestExecutionMergeCoordinatorConsumesOnceAndAudits(t *testing.T) {
	a, r := executionTestApp(t)
	r.LeaseRequired = true
	a.cfg.GitHub.AllowedOrgs = []string{defaultOrg}
	now := time.Now()
	head := strings.Repeat("a", 40)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Request = r
		st.Starts[0].Execution.Lease = &execution.LeaseGrant{ExpiresAt: now.Add(time.Minute)}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	oldRead, oldMerge := readLoopPull, runExecutionMerge
	t.Cleanup(func() { readLoopPull, runExecutionMerge = oldRead, oldMerge })
	readLoopPull = func(context.Context, string) (superloop.Pull, error) {
		return superloop.Pull{State: superloop.Open, Head: head, Green: true, Reviews: []superloop.Review{{Head: head, Verdict: superloop.Approved, At: now}}}, nil
	}
	use := execution.MergeUse{ID: "grant-test", Ref: r.Ref, Repository: r.Repository, PR: 7, Head: head}
	grant := execution.MergeAuthorization{ID: use.ID, Ref: use.Ref, Repository: use.Repository, PR: use.PR, Head: head, Issuer: supervisorName, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err := execution.WriteMergeAuthorization(a.mergeAuthorityRoot(), grant, "issued", now); err != nil {
		t.Fatal(err)
	}
	calls := 0
	runExecutionMerge = func(ctx context.Context, _ execution.MergeUse) error {
		calls++
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 20*time.Second {
			t.Fatal("merge not bounded by live callback")
		}
		return errors.New("uncertain merge outcome")
	}
	var result execution.MergeResult
	if err := coordinatorTestCall(t, a, r, runMerge, use, &result, false); err == nil {
		t.Fatal("uncertain merge reported success")
	}
	if err := coordinatorTestCall(t, a, r, runMerge, use, &result, false); err == nil || calls != 1 {
		t.Fatal("merge repeated after uncertain response")
	}
	grant, err := execution.ReadMergeAuthorization(a.mergeAuthorityRoot(), use.ID)
	if err != nil || !grant.Consumed || grant.Outcome != "uncertain" {
		t.Fatal("consumption missing", err)
	}
	events, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "execution.merge-denied" })
	if err != nil || len(events) != 2 {
		t.Fatal("denial audits missing", err)
	}
}

func TestExecutionMergeAuthorityCannotBeGrantedWithoutDeskReview(t *testing.T) {
	a, r := executionTestApp(t)
	if _, err := a.issueExecutionMerge(context.Background(), r.Ref, r.Repository, 7, strings.Repeat("a", 40), state.Party{Name: r.Name}); err == nil {
		t.Fatal("worker issued own authorization")
	}
}

func TestExecutionMergeLiveDeniesStopHoldMovedHeadAndMissingLease(t *testing.T) {
	a, r := executionTestApp(t)
	r.LeaseRequired = true
	a.cfg.GitHub.AllowedOrgs = []string{defaultOrg}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	st.Starts[0].Execution.Request = r
	st.Starts[0].Execution.Lease = &execution.LeaseGrant{ExpiresAt: time.Now().Add(time.Minute)}
	head := strings.Repeat("a", 40)
	use := execution.MergeUse{ID: "grant", Ref: r.Ref, Repository: r.Repository, PR: 7, Head: head}
	old := readLoopPull
	t.Cleanup(func() { readLoopPull = old })
	pull := superloop.Pull{State: superloop.Open, Head: head, Green: true, Reviews: []superloop.Review{{Head: head, Verdict: superloop.Approved}}}
	readLoopPull = func(context.Context, string) (superloop.Pull, error) { return pull, nil }
	if err := a.executionMergeLive(context.Background(), st, use); err != nil {
		t.Fatal(err)
	}
	st.Stop = &state.Stop{Reason: "merge-stop-fixture"}
	if a.executionMergeLive(context.Background(), st, use) == nil {
		t.Fatal("global stop admitted merge")
	}
	st.Stop = nil
	st.Holds = []state.Hold{{Target: holdMerges}}
	if a.executionMergeLive(context.Background(), st, use) == nil {
		t.Fatal("global hold admitted merge")
	}
	st.Holds = nil
	pull.Head = strings.Repeat("b", 40)
	if a.executionMergeLive(context.Background(), st, use) == nil {
		t.Fatal("moved head admitted merge")
	}
	pull.Head = head
	pull.Green = false
	if a.executionMergeLive(context.Background(), st, use) == nil {
		t.Fatal("red CI admitted merge")
	}
	pull.Green = true
	st.Starts[0].Execution.Lease = nil
	if a.executionMergeLive(context.Background(), st, use) == nil {
		t.Fatal("missing lease admitted merge")
	}
}

func TestExecutionMergeGrantRequiresSupervisorAndReviewRound(t *testing.T) {
	a, r := executionTestApp(t)
	a.cfg.GitHub.AllowedOrgs = []string{defaultOrg}
	r.LeaseRequired = true
	now := time.Now()
	head := strings.Repeat("a", 40)
	supervisor := state.Party{Session: "supervisor-session", Name: supervisorName}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Supervisor = &state.Supervisor{Party: supervisor}
		st.Starts[0].Execution.Request = r
		st.Starts[0].Execution.Lease = &execution.LeaseGrant{ExpiresAt: now.Add(time.Minute)}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	reviewEvent := about(event(loopParty, "supervisor.review", "[review %s#7@%s] reviewer: independent round", r.Repository, head), r.Repository+"#7")
	reviewEvent.At = now.Add(-time.Minute)
	if err := a.store.Record(reviewEvent); err != nil {
		t.Fatal(err)
	}
	old := readLoopPull
	t.Cleanup(func() { readLoopPull = old })
	readLoopPull = func(context.Context, string) (superloop.Pull, error) {
		return superloop.Pull{State: superloop.Open, Head: head, Green: true, Reviews: []superloop.Review{{Head: head, Verdict: superloop.Approved, At: time.Now().Add(time.Second)}}}, nil
	}
	if _, err := a.issueExecutionMerge(context.Background(), r.Ref, r.Repository, 7, head, state.Party{Name: r.Name}); err == nil {
		t.Fatal("worker impersonated supervisor")
	}
	grant, err := a.issueExecutionMerge(context.Background(), r.Ref, r.Repository, 7, head, supervisor)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := execution.ReadMergeAuthorization(a.mergeAuthorityRoot(), grant.ID)
	if err != nil || stored.Issuer != supervisor.Name || stored.Ref != r.Ref {
		t.Fatal("grant identity not durable", err)
	}
}

func TestExecutionMergeRetryReadsResultWithoutRepeatingMutation(t *testing.T) {
	a, r := executionTestApp(t)
	now := time.Now()
	head := strings.Repeat("a", 40)
	use := execution.MergeUse{ID: "consumed-grant", Ref: r.Ref, Repository: r.Repository, PR: 7, Head: head}
	grant := execution.MergeAuthorization{ID: use.ID, Ref: use.Ref, Repository: use.Repository, PR: use.PR, Head: use.Head, Issuer: supervisorName, IssuedAt: now.Add(-time.Minute), ExpiresAt: now, Consumed: true, Outcome: "uncertain"}
	if err := execution.WriteMergeAuthorization(a.mergeAuthorityRoot(), grant, "used", now); err != nil {
		t.Fatal(err)
	}
	oldRead, oldMerge := readLoopPull, runExecutionMerge
	t.Cleanup(func() { readLoopPull, runExecutionMerge = oldRead, oldMerge })
	reads := 0
	readLoopPull = func(context.Context, string) (superloop.Pull, error) {
		reads++
		return superloop.Pull{State: superloop.Merged, Head: head}, nil
	}
	runExecutionMerge = func(context.Context, execution.MergeUse) error {
		t.Fatal("consumed grant mutated GitHub again")
		return nil
	}
	var result execution.MergeResult
	if err := coordinatorTestCall(t, a, r, runMerge, use, &result, false); err != nil || result.Outcome != execution.MergeSucceeded || reads != 1 {
		t.Fatal("retry did not read uncertain outcome", err)
	}
}

func TestExecutionMergeSharesLaneAndPreservesObservedSettlement(t *testing.T) {
	a, r := executionTestApp(t)
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	use := execution.MergeUse{ID: "lane-grant", Ref: r.Ref, Repository: r.Repository, PR: 7, Head: strings.Repeat("a", 40)}
	a.recordExecutionMergeLane(st, use, "uncertain", st.Agents[0].Party)
	if len(st.Merges) != 1 || st.Merges[0].Phase != state.Waiting || !st.Merges[0].Outside {
		t.Fatal("uncertain merge lost its lane reservation")
	}
	a.recordExecutionMergeLane(st, use, execution.MergeSucceeded, st.Agents[0].Party)
	finished := st.Merges[0].Finished
	a.recordExecutionMergeLane(st, use, "uncertain", st.Agents[0].Party)
	if len(st.Merges) != 1 || st.Merges[0].Phase != state.Settling || !st.Merges[0].Finished.Equal(finished) {
		t.Fatal("reconciliation reset or lost settlement")
	}
}
