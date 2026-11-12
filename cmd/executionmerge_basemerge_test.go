package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// An SSH execution's authenticated approval of H0 carries to H1 when a
// worker merges the base into H0. After a coordinator restart, a green H1
// can merge without another review; other changes do not inherit approval.
func TestExecutionApprovalCoversAWorkersBaseUpdateAcrossARestart(t *testing.T) {
	a, use, pull := mergeReviewRig(t)
	// This fixture isolates authenticated approval inheritance, not Git/CI.
	prevCheck, prevQueue := reviewedMergeCheck, readReviewedMergeQueue
	t.Cleanup(func() { reviewedMergeCheck, readReviewedMergeQueue = prevCheck, prevQueue })
	reviewedMergeCheck = func(context.Context, *app, superloop.Pull, string) (bool, string, int) { return false, "", 0 }
	readReviewedMergeQueue = func(context.Context, superloop.Pull) (bool, error) { return true, nil }
	h0, h1 := use.Head, strings.Repeat("c", 40)
	ref := fmt.Sprintf("%s#%d", use.Repository, use.PR)
	approved := superloop.Review{ID: "independent-approval", Verdict: superloop.Approved}
	completeMergeReview(t, a, use, &approved)
	approved.Member = true
	pull.Reviews = append([]superloop.Review{approved}, pull.Reviews...)
	pull.Ref, pull.Base, pull.MergeState, pull.Checks = ref, mainBranch, mergeStateBehind, []string{behindBuild}

	covers := true
	baseMerge, behind, command := ReadBaseMerge, readReviewedBehind, LoopCommand
	t.Cleanup(func() {
		ReadBaseMerge, readReviewedBehind, LoopCommand = baseMerge, behind, command
	})
	ReadBaseMerge = func(_ context.Context, p superloop.Pull, reviewed string) (bool, error) {
		return covers && reviewed == h0 && p.Head == h1, nil
	}
	readReviewedBehind = func(context.Context, superloop.Pull) (int, error) { return 0, nil }
	var merges [][]string
	LoopCommand = func(_ context.Context, _ string, argv []string) (string, string, int, error) {
		switch {
		case len(argv) > 2 && argv[0] == "gh" && argv[2] == superloop.KindMerge:
			merges = append(merges, argv)
			pull.State, pull.MergeCommit = superloop.Merged, strings.Repeat("d", 40)
		}
		return "", "", 0, nil
	}
	worker := stWorkerName(t, a)
	claim := func(head string) {
		t.Helper()
		if err := a.store.Record(about(event(LoopParty, LoopClaimVerb, "[go %s@%s] %s", ref, head, worker), ref)); err != nil {
			t.Fatal(err)
		}
	}

	// A worker merges the base and pushes H1 while its checks are pending.
	pull.Head, pull.Green = h1, false
	reopened, err := state.Open(a.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	a.store = reopened
	pull.Green, pull.Checks = true, []string{behindBuild}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	use.ID, use.Head = "", h1
	covers = false
	if err := a.executionMergeLive(t.Context(), st, use); err == nil {
		t.Fatal("a head changing more than the base inherited the approval")
	}
	covers = true
	if err := a.executionMergeLive(t.Context(), st, use); err != nil {
		t.Fatalf("the approval of %s does not cover its base merge %s: %v", h0, h1, err)
	}
	claim(h1)
	doc := a.reviewedMerge(t.Context(), use.Repository, use.PR, h1)
	if doc.ExitCode != 0 || len(merges) != 1 || !slices.Contains(merges[0], h1) {
		t.Fatalf("later merge of the covered head: %+v, merges %v", doc, merges)
	}
	if st, err = a.store.Read(); err != nil {
		t.Fatal(err)
	}
	if len(st.DeskReviews) != 1 {
		t.Fatalf("%d review rounds, want the one approval", len(st.DeskReviews))
	}
}
