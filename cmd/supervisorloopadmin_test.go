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
