package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const adminPolicyTestReason = "the base branch policy prohibits the merge: add the --admin flag"

func TestLoopMergePolicyRefusalRetriesAfterAdminConfig(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	attempts := 0
	r.command = func(argv []string) (string, int) {
		attempts++
		if slices.Contains(argv, "--admin") {
			t.Fatalf("retry bypassed the server-side queue: %v", argv)
		}
		if attempts <= 2 {
			return adminPolicyTestReason, 1
		}
		r.pull.State = superloop.Merged
		r.pull.MergeCommit = loopFixed
		return "", 0
	}
	r.pass(t)
	r.pass(t)
	if attempts != 2 {
		t.Fatalf("configuration refusal did not retry next round: attempts=%d output=%s", attempts, r.out)
	}
	if strings.Count(r.out.String(), "merge.admin") != 1 {
		t.Fatalf("configuration refusal must name merge.admin once: %s", r.out)
	}
	if wake := r.called(agentsName, agentWakeName, loopWorker); wake != nil {
		t.Fatalf("configuration refusal woke worker: %v", wake)
	}
	// Model the policy/configuration correction between rounds. The newer
	// queue invariant forbids using --admin to bypass server-side checks;
	// the corrected policy must admit the ordinary queue mutation.
	r.a.cfg.Merge.Admin = []string{loopMergeRepo}
	r.pass(t)
	if attempts != 3 || r.pull.State != superloop.Merged {
		t.Fatalf("approved head not merged next round after config change: attempts=%d state=%s output=%s", attempts, r.pull.State, r.out)
	}
	if len(r.logged(t, "supervisor.merge")) != 1 || len(r.logged(t, "supervisor.go")) != 0 {
		t.Fatalf("expected coordinator merge without worker go: %s", r.out)
	}
	if review := r.called(agentsName, agentStartName, superloop.ReviewerName(loopPR, 2)); review != nil {
		t.Fatalf("configuration change required another review: %v", review)
	}
}

func TestReviewedMergeAdminPolicyPrecedesBranchRepair(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.a.cfg.Merge.Admin = []string{loopMergeRepo}
	r.command = func([]string) (string, int) {
		r.pull.MergeState = "BEHIND"
		return adminPolicyTestReason, 1
	}
	r.pass(t)
	if wake := r.called(agentsName, agentWakeName, loopWorker); wake != nil {
		t.Fatalf("policy refusal misclassified as branch repair: %v", wake)
	}
	// Inspect the mutation document directly under the pending exact-head claim.
	r.pull.MergeState = "CLEAN"
	doc := r.a.reviewedMerge(context.Background(), loopMergeRepo, 7, loopHead)
	if doc.ExitCode != github.SquashRefused {
		t.Fatalf("policy refusal classified as %d: %+v", doc.ExitCode, doc)
	}
}

func TestLoopMergeRetriesSavedAdminPolicyFailure(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Failed = append(st.Failed, state.Failed{Repo: loopMergeRepo, PR: 7, By: loopParty, Exit: github.SquashNotApplicable, Reason: adminPolicyTestReason})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.command = func([]string) (string, int) {
		r.pull.State = superloop.Merged
		r.pull.MergeCommit = loopFixed
		return "", 0
	}
	r.pass(t)
	if r.pull.State != superloop.Merged {
		t.Fatalf("saved configuration refusal prevented retry: %s", r.out)
	}
	if wake := r.called(agentsName, agentWakeName, loopWorker); wake != nil {
		t.Fatalf("saved configuration refusal requested repair: %v", wake)
	}
}

// seedOlderRepair records what a loop before the configuration-refusal fix
// logged for an admin policy refusal: a branch repair stage for the go and
// the go itself completed as "branch repair requested".
func seedOlderRepair(t *testing.T, r *loopRig, reason string) {
	t.Helper()
	key := "go " + loopPR + "@" + loopHead
	for _, e := range []state.Event{
		about(event(loopParty, loopStageVerb, "[%s repair] (exit %d): %s", key, github.SquashNotApplicable, reason), loopPR),
		about(event(loopParty, "supervisor."+superloop.KindMerge, "[%s] %s: merge failed; branch repair requested", key, loopWorker), loopPR),
	} {
		if err := r.a.store.Record(e); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoopRestartRecoversOlderAdminPolicyRepair(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	seedOlderRepair(t, r, adminPolicyTestReason)
	attempts := 0
	r.command = func(argv []string) (string, int) {
		attempts++
		if slices.Contains(argv, "--admin") {
			t.Fatalf("retry bypassed the server-side queue: %v", argv)
		}
		if len(r.a.cfg.Merge.Admin) == 0 {
			return adminPolicyTestReason, 1
		}
		r.pull.State = superloop.Merged
		r.pull.MergeCommit = loopFixed
		return "", 0
	}
	r.pass(t)
	if attempts != 1 {
		t.Fatalf("older policy repair stage kept the approved head out of the lane: attempts=%d output=%s", attempts, r.out)
	}
	if len(r.logged(t, loopUnstageVerb)) != 1 || len(r.logged(t, loopClaimEndVerb)) != 1 {
		t.Fatalf("obsolete repair stage and go not closed durably: %s", r.out)
	}
	r.a.cfg.Merge.Admin = []string{loopMergeRepo}
	r.pass(t)
	if attempts != 2 || r.pull.State != superloop.Merged {
		t.Fatalf("approved head not merged after the configuration change: attempts=%d state=%s output=%s", attempts, r.pull.State, r.out)
	}
	if wake := r.called(agentsName, agentWakeName, loopWorker); wake != nil {
		t.Fatalf("recovery woke the worker: %v", wake)
	}
	if review := r.called(agentsName, agentStartName, superloop.ReviewerName(loopPR, 2)); review != nil {
		t.Fatalf("recovery required another review: %v", review)
	}
	if n := len(r.logged(t, loopUnstageVerb)); n != 1 {
		t.Fatalf("obsolete repair closed %d times", n)
	}
}

func TestLoopRestartKeepsGenuineBranchRepair(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	seedOlderRepair(t, r, "branch is behind its base")
	attempts := 0
	r.command = func([]string) (string, int) {
		attempts++
		return "", 0
	}
	r.pass(t)
	if attempts != 0 || len(r.logged(t, loopUnstageVerb)) != 0 || len(r.logged(t, loopClaimEndVerb)) != 0 {
		t.Fatalf("genuine branch repair was undone: attempts=%d output=%s", attempts, r.out)
	}
}

func TestLoopDryRunPreservesOlderAdminPolicyRepair(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	seedOlderRepair(t, r, adminPolicyTestReason)
	before, err := r.a.store.Events(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.a.dryRunSupervisorLoop(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := r.a.store.Events(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("dry run appended %d events, including %d unstages and %d unclaims", len(after)-len(before), len(r.logged(t, loopUnstageVerb)), len(r.logged(t, loopClaimEndVerb)))
	}
	if !strings.Contains(r.out.String(), "SUPERVISOR WOULD GO") {
		t.Fatalf("dry run did not preview the recovered merge: %s", r.out)
	}
	// A subsequent real pass must still perform and record the recovery.
	r.command = func([]string) (string, int) { return adminPolicyTestReason, 1 }
	r.pass(t)
	if len(r.logged(t, loopUnstageVerb)) != 1 || len(r.logged(t, loopClaimEndVerb)) != 1 {
		t.Fatalf("real pass did not close the saved repair exactly once: %s", r.out)
	}
}
