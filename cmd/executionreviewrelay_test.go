package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func reviewRelayRig(t *testing.T) (*app, execution.StartRequest, *superloop.Pull) {
	t.Helper()
	a, r := executionTestApp(t)
	a.cfg.GitHub.AllowedOrgs = []string{defaultOrg}
	r.LeaseRequired = true
	head := strings.Repeat("a", 40)
	ref := r.Repository + "#7"
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		start := &st.Starts[0]
		start.Name = superloop.ReviewerName(ref, 1)
		start.By = loop.LoopParty
		start.At = time.Now().Add(-time.Minute)
		start.Execution.Request = r
		start.Execution.Handle.Commit = head
		start.Execution.Lease = &execution.LeaseGrant{ExpiresAt: time.Now().Add(time.Minute)}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.assignExecutionReview(context.Background(), superloop.Action{Ref: ref, Head: head, Round: 1, Agent: "local-author"}); err != nil {
		t.Fatal(err)
	}
	pull := &superloop.Pull{Ref: ref, Head: head, State: superloop.Open}
	old := loop.ReadLoopPull
	t.Cleanup(func() { loop.ReadLoopPull = old })
	loop.ReadLoopPull = func(context.Context, string) (superloop.Pull, error) { return *pull, nil }
	return a, r, pull
}

func TestExecutionReviewRelayReadsAndPostsAssignedHead(t *testing.T) {
	a, r, pull := reviewRelayRig(t)
	old := github.RunGH
	t.Cleanup(func() { github.RunGH = old })
	body := "Review: approved\nHead reviewed: `" + pull.Head + "`\nChecked the fix."
	url := "https://github.com/" + r.Repository + "/pull/7#pullrequestreview-100"
	posts := 0
	github.RunGH = func(_ context.Context, args ...string) ([]byte, error) {
		switch strings.Join(args[:2], " ") {
		case "pr view":
			return []byte(`{"closingIssuesReferences":[{"url":"https://github.com/giantswarm/beekeeper/issues/1"}]}`), nil
		case "issue view":
			if strings.Join(args, " ") != "issue view 1 --repo giantswarm/beekeeper --json url,title,body,comments" {
				t.Fatalf("wrong linked issue: %v", args)
			}
			return []byte(`{"body":"issue requirement"}`), nil
		case "pr diff":
			return []byte("assigned diff"), nil
		default:
			if len(args) == 4 && args[0] == workerGitHubAPIGroup && args[2] == "--paginate" && args[3] == "--slurp" {
				if posts == 0 {
					return []byte(`[[]]`), nil
				}
				return json.Marshal([][]map[string]any{{{"body": body, "commit_id": pull.Head, "html_url": url, "submitted_at": time.Now().UTC().Format(time.RFC3339)}}})
			}
			want := []string{workerGitHubAPIGroup, "repos/" + r.Repository + "/pulls/7/reviews", "--method", "POST", reviewRelayRawField, "event=COMMENT", reviewRelayRawField, "commit_id=" + pull.Head, reviewRelayRawField, "body=" + body}
			if fmt.Sprint(args) != fmt.Sprint(want) {
				t.Fatalf("unscoped review write: %v", args)
			}
			posts++
			pull.Reviews = []superloop.Review{{Body: body, Commit: pull.Head, URL: url, At: time.Now()}}
			return []byte(`{"html_url":"` + url + `"}`), nil
		}
	}
	for _, request := range []executionReviewRequest{{}, {Diff: true}, {Body: body}, {Body: body}} {
		var result string
		drop := request.Body != "" && posts == 0
		err := coordinatorTestCall(t, a, r, executionReviewRelay, request, &result, drop)
		if drop {
			if err == nil {
				t.Fatal("lost review response reported success")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if request.Body != "" && result != url {
			t.Fatalf("wrong review receipt %q", result)
		}
		if request.Diff && result != "assigned diff" {
			t.Fatalf("wrong diff %q", result)
		}
		if !request.Diff && request.Body == "" && !strings.Contains(result, "issue requirement") {
			t.Fatalf("missing issue: %s", result)
		}
	}
	if posts != 1 {
		t.Fatalf("retry posted %d reviews", posts)
	}
}

func TestExecutionReviewRelayRefusesUnassignedExpiredAndChangedHeads(t *testing.T) {
	for _, failure := range []string{"unassigned", "expired review lease", "changed review head", "closed review pull", "held review publication", "bad body", "bad caller", "oversized", "mixed request", workerGitHubCompleted, reviewedStoppedCase, "outside allowlist", "global stop"} {
		t.Run(failure, func(t *testing.T) {
			a, r, pull := reviewRelayRig(t)
			request := executionReviewRequest{}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				switch failure {
				case reviewedStoppedCase:
					st.Starts[0].Execution.StopRequest = &execution.StopRequest{}
				case "outside allowlist":
					a.cfg.GitHub.AllowedOrgs = nil
				case "global stop":
					st.Stop = &state.Stop{}
				case "unassigned":
					st.DeskReviews = nil
				case "expired review lease":
					st.Starts[0].Execution.Lease.ExpiresAt = time.Now().Add(-time.Minute)
				case "held review publication":
					st.Holds = append(st.Holds, state.Hold{Target: holdGitHub})
				case workerGitHubCompleted:
					st.Starts[0].Execution.Completed = true
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "changed review head":
				pull.Head = strings.Repeat("b", 40)
			case "closed review pull":
				pull.State = superloop.Merged
			case "bad body":
				request.Body = "Review: approved\nHead reviewed: `" + strings.Repeat("b", 40) + "`"
			case "bad caller":
				r.ControlKey = "wrong-key"
			case "oversized":
				request.Body = strings.Repeat("x", (64<<10)+1)
			case "mixed request":
				request.Diff, request.Body = true, "Review: approved"
			}
			var result string
			if err := coordinatorTestCall(t, a, r, executionReviewRelay, request, &result, false); err == nil {
				t.Fatal("unauthorized review relay accepted")
			}
		})
	}
}
