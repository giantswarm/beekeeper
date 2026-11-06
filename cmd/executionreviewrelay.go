package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const executionReviewRelay = "review-relay"
const reviewRelayRawField = "--raw-field"

type executionReviewRequest struct {
	Diff bool   `json:"diff,omitempty"`
	Body string `json:"body,omitempty"`
}

// This relay accepts content, never a command or a coordinator file path.
// The authenticated execution's assigned round supplies every write target.
func (a *app) relayExecutionReview(ctx context.Context, st *state.State, ref execution.Ref, request executionReviewRequest) (string, error) {
	if _, err := a.executionPublicationLive(st, ref); err != nil {
		return "", err
	}
	_, i := executionRecord(st, ref)
	var round *state.DeskReview
	roundIndex := -1
	for n := len(st.DeskReviews) - 1; n >= 0; n-- {
		candidate := &st.DeskReviews[n]
		if sameReviewSession(candidate.Reviewer, st.Starts[i].Party) {
			round = candidate
			roundIndex = n
			break
		}
	}
	if round == nil {
		return "", refused("execution has no assigned review round yet")
	}
	r, ok := parseRef(round.Ref)
	if !ok {
		return "", refused("invalid assigned review reference")
	}
	if request.Diff && request.Body != "" || len(request.Body) > 64<<10 {
		return "", usageErr("review accepts a diff read or a review body up to 64 KiB")
	}
	pull, err := readLoopPull(ctx, round.Ref)
	if err != nil {
		return "", err
	}
	if pull.State != superloop.Open || (request.Body == "" && pull.Head != round.Head) {
		return "", refused("assigned pull request head changed or is no longer open")
	}
	number := strconv.Itoa(r.N)
	if request.Body != "" {
		verdict := superloop.Verdict("COMMENTED", request.Body)
		reviewedHead := superloop.ReviewedHead(request.Body, "")
		headOK := reviewedHead == round.Head && pull.Head == round.Head
		if !headOK && verdict == superloop.Approved && reviewedHead != "" && reviewedHead == pull.Head {
			headOK = true
			for _, prev := range st.DeskReviews[:roundIndex] {
				if strings.EqualFold(prev.Ref, round.Ref) && (prev.Head == pull.Head || prev.Result != nil && prev.Result.Head == pull.Head) {
					headOK = false
					break
				}
			}
		}
		if verdict == "" || !headOK {
			return "", refused("review must begin with Review: approved or Review: changes requested and name the assigned Head reviewed")
		}
		// Reconcile a lost response before posting again. Only this round's exact
		// content, commit and time qualify; completion still authenticates its URL.
		path := "repos/" + r.Repo + "/pulls/" + number + "/reviews"
		raw, err := github.RunGH(ctx, workerGitHubAPIGroup, path, "--paginate", "--slurp")
		if err != nil {
			return "", err
		}
		var pages [][]struct {
			Body   string    `json:"body"`
			Commit string    `json:"commit_id"`
			URL    string    `json:"html_url"`
			At     time.Time `json:"submitted_at"`
		}
		if err := json.Unmarshal(raw, &pages); err != nil {
			return "", err
		}
		for _, page := range pages {
			for _, review := range page {
				if review.Body == request.Body && review.Commit == reviewedHead && !review.At.Before(round.AssignedAt.Add(-30*time.Second)) && review.URL != "" {
					return review.URL, nil
				}
			}
		}
		out, err := github.RunGH(ctx, workerGitHubAPIGroup, "repos/"+r.Repo+"/pulls/"+number+"/reviews", "--method", "POST", reviewRelayRawField, "event=COMMENT", reviewRelayRawField, "commit_id="+reviewedHead, reviewRelayRawField, "body="+request.Body)
		if err != nil {
			return "", err
		}
		var posted struct {
			URL string `json:"html_url"`
		}
		if err := json.Unmarshal(out, &posted); err != nil {
			return "", err
		}
		if posted.URL == "" {
			return "", refused("GitHub returned no review URL")
		}
		return posted.URL, nil
	}
	if request.Diff {
		out, err := github.RunGH(ctx, "pr", "diff", number, "--repo", r.Repo)
		return string(out), err
	}
	out, err := github.RunGH(ctx, "pr", "view", number, "--repo", r.Repo, "--json", "url,title,body,headRefOid,baseRefName,closingIssuesReferences")
	if err != nil {
		return "", err
	}
	var details struct {
		Issues []struct {
			URL string `json:"url"`
		} `json:"closingIssuesReferences"`
	}
	if err := json.Unmarshal(out, &details); err != nil {
		return "", err
	}
	text := string(out)
	for _, issue := range details.Issues {
		// GitHub supplied the link, but still require an explicit GitHub issue URL.
		match := briefURL.FindStringSubmatch(issue.URL)
		if len(match) != 3 || match[0] != issue.URL || !strings.Contains(issue.URL, "/issues/") {
			return "", refused("invalid linked issue URL")
		}
		body, err := github.RunGH(ctx, "issue", "view", match[2], "--repo", match[1], "--json", "url,title,body,comments")
		if err != nil {
			return "", err
		}
		text += "\n" + string(body)
	}
	return text, nil
}

func (a *app) executionReviewCmd() *cobra.Command {
	var diff bool
	var bodyPath string
	c := &cobra.Command{Use: "review", Short: "Read or post the assigned execution review through the coordinator", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		s, j, err := a.executionCaller()
		if err != nil {
			return err
		}
		request := executionReviewRequest{Diff: diff}
		if bodyPath != "" {
			if diff {
				return usageErr("--diff and --body-file are exclusive")
			}
			if err := a.sandboxPaths([]string{bodyPath}, nil); err != nil {
				return err
			}
			raw, err := os.ReadFile(bodyPath) //nolint:gosec // caller-supplied host file, checked by sandboxPaths
			if err != nil {
				return err
			}
			if len(raw) == 0 || len(raw) > 64<<10 {
				return usageErr("review body must contain 1 to 65536 bytes")
			}
			request.Body = string(raw)
		}
		co := s.Config.Coordinators[j.Request.Coordinator]
		var result string
		if err := execution.CoordinatorCall(cmd.Context(), co.Callback, string(j.Handle.Ref.Host), executionReviewRelay, j.Handle.Ref, request, j.Request.ControlKey, &result, s.Callback); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), result)
		return err
	}}
	c.Flags().BoolVar(&diff, "diff", false, "read the assigned pull request diff")
	c.Flags().StringVar(&bodyPath, "body-file", "", "post a review from this host file")
	return c
}
