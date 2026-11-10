package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func TestReviewWriteUsesRecordedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		start    state.Start
		assigned bool
		want     bool
	}{
		{"kind", state.Start{Kind: taskKindReview, Party: state.Party{Session: "review"}}, false, true},
		{"assignment", state.Start{Party: state.Party{Session: "review"}}, true, true},
		{"worker", state.Start{Party: state.Party{Session: "review"}}, false, false},
		{"forged name", state.Start{Kind: taskKindReview, Party: state.Party{Name: "review", Session: "other"}}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &state.State{Starts: []state.Start{tc.start}}
			if tc.assigned {
				st.DeskReviews = []state.DeskReview{{Reviewer: tc.start.Party}}
			}
			if got := reviewerSession(st, state.Party{Session: "review"}); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestReviewWriteHookAndHostIdentity(t *testing.T) {
	a, _ := executionTestApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = []state.Start{{Kind: taskKindReview, Party: state.Party{Session: "original", HostSession: "review-host-session"}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "review-host-session")
	if got := a.hookReviewWrite("resumed"); !strings.Contains(got, "agents idle --done --report") {
		t.Fatal(got)
	}
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
	if got := a.hookReviewWrite("worker"); got != "" {
		t.Fatal(got)
	}
}

func TestExecutionMergeRejectsLegacySelfFixedResult(t *testing.T) {
	a, use, pull := mergeReviewRig(t)
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	round := executionReviewRound(st, use)
	head := strings.Repeat("c", 40)
	round.Result = &state.DeskReviewResult{Head: head, By: round.Reviewer, Verdict: superloop.Approved, ReportedAt: time.Now()}
	use.Head, pull.Head = head, head
	for _, loopClaim := range []bool{false, true} {
		if err := a.authenticateReviewResult(context.Background(), st, use, *pull, loopClaim); err == nil {
			t.Fatal("legacy self-fixed review authorized a merge")
		}
	}
}
