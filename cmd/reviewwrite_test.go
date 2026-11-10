package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func TestReviewWriteUsesRecordedIdentity(t *testing.T) {
	const reviewerID = "reviewwrite-reviewer-id"
	for _, tc := range []struct {
		name     string
		start    state.Start
		assigned bool
		want     bool
	}{
		{"reviewwrite-kind", state.Start{Kind: taskKindReview, Party: state.Party{Session: reviewerID}}, false, true},
		{"reviewwrite-assignment", state.Start{Party: state.Party{Session: reviewerID}}, true, true},
		{"reviewwrite-worker", state.Start{Party: state.Party{Session: reviewerID}}, false, false},
		{"reviewwrite-forged-name", state.Start{Kind: taskKindReview, Party: state.Party{Name: taskKindReview, Session: "reviewwrite-other-session"}}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &state.State{Starts: []state.Start{tc.start}}
			if tc.assigned {
				st.DeskReviews = []state.DeskReview{{Reviewer: tc.start.Party}}
			}
			if got := reviewerSession(st, state.Party{Session: reviewerID}); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestReviewWriteHookAndHostIdentity(t *testing.T) {
	const hostID = "reviewwrite-host-session"
	t.Setenv(codex.EnvAgent, "")
	t.Setenv(omp.EnvAgent, "")
	t.Setenv(agy.EnvAgent, "")
	a, _ := executionTestApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = []state.Start{{Kind: taskKindReview, Party: state.Party{Session: "reviewwrite-original", HostSession: hostID}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	a.store = nil
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", hostID)
	if got := a.hookReviewWrite("reviewwrite-resumed"); !strings.Contains(got, "agents idle --done --report") {
		t.Fatal(got)
	}
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
	if got := a.hookReviewWrite("reviewwrite-worker-session"); got != "" {
		t.Fatal(got)
	}
}

func TestReviewWriteHookUsesHarnessIdentity(t *testing.T) {
	const agentID = "reviewwrite-harness-agent"
	for _, harness := range []struct{ variable, prefix string }{
		{codex.EnvAgent, codex.HostPrefix},
		{omp.EnvAgent, omp.HostPrefix},
		{agy.EnvAgent, agy.HostPrefix},
	} {
		t.Run(harness.prefix, func(t *testing.T) {
			t.Setenv(codex.EnvAgent, "")
			t.Setenv(omp.EnvAgent, "")
			t.Setenv(agy.EnvAgent, "")
			t.Setenv(harness.variable, agentID)
			t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "reviewwrite-launcher")
			a, _ := executionTestApp(t)
			for _, kind := range []string{taskKindReview, workerRole} {
				if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.Starts = []state.Start{
						{Kind: kind, Party: state.Party{HostSession: harness.prefix + agentID}},
						{Kind: taskKindReview, Party: state.Party{HostSession: "reviewwrite-launcher"}},
					}
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
				for _, session := range []string{"reviewwrite-conversation", ""} {
					got := a.hookReviewWrite(session)
					if kind == taskKindReview && got != reviewWriteRefusal {
						t.Fatalf("reviewer identity was missed: %q", got)
					}
					if kind == workerRole && got != "" {
						t.Fatalf("worker inherited launcher's review identity: %q", got)
					}
				}
			}
		})
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
