package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// A reviewer on an SSH execution host fixes its own findings: it commits in
// its workspace and publishes the fast-forward to the reviewed pull request's
// branch, with `beekeeper agents publish` or a push through its scoped
// credential, then approves the head it pushed
// (https://github.com/giantswarm/beekeeper/issues/3849).

// reviewWriteUnassigned refuses a review session's write before the loop has
// recorded its assignment: only the assignment names the branch it may push.
const reviewWriteUnassigned = "Refused: this review session has no recorded review assignment yet, and only its assignment names the pull request branch it may push to; retry once the supervisor has recorded the start."

// reviewWriteNoMetadata refuses a reviewer's title or body: it pushes to the
// pull request it reviews and never opens or edits one.
const reviewWriteNoMetadata = "Refused: a reviewer publishes its fixes to the reviewed pull request's branch with beekeeper agents publish alone; it opens no pull request and edits no title or body."

// executionReviewWriteBranch is the branch a reviewer's fixes go to: the head
// branch of the open pull request its live review round names. The grant
// comes from the loop's durable assignment and the reviewer's own execution,
// never from a ref the host names. A round with a result, a released or a
// superseded round grants nothing. Callers also require a live lease.
func executionReviewWriteBranch(ctx context.Context, st *state.State, ref execution.Ref) (string, error) {
	r, i := executionRecord(st, ref)
	round := assignedExecutionReviewRound(st, ref)
	if r == nil || r.Completed || r.Terminal() || r.Lease == nil || !r.Lease.ExpiresAt.After(time.Now()) || i < 0 || round == nil || round.ID == "" || round.Round <= 0 || round.Result != nil || !round.Released.IsZero() || round.Head != r.Handle.Commit {
		return "", refused("no live review assignment grants this execution its pull request's branch")
	}
	for n := len(st.DeskReviews) - 1; n >= 0; n-- {
		if candidate := &st.DeskReviews[n]; strings.EqualFold(candidate.Ref, round.Ref) {
			if candidate.ID != round.ID {
				return "", refused("review branch assignment was superseded")
			}
			break
		}
	}
	pr, ok := parseRef(round.Ref)
	if !ok || !strings.EqualFold(pr.Repo, r.Request.Repository) || !sameReviewSession(round.Reviewer, st.Starts[i].Party) {
		return "", refused("review branch assignment identity mismatch")
	}
	pull, err := loop.ReadLoopPull(ctx, round.Ref)
	if err != nil {
		return "", err
	}
	if !r.Lease.ExpiresAt.After(time.Now()) {
		return "", refused("review branch assignment expired during revalidation")
	}
	if pull.State != superloop.Open || !strings.EqualFold(pull.Ref, round.Ref) || pull.Branch == "" || pull.Branch != executionStartedBranch(r) {
		return "", refused("review branch assignment no longer matches the open pull request")
	}
	return pull.Branch, nil
}

// executionStartedBranch is the branch an execution was started on: its
// request's ref, or, once the coordinator pinned the source to that ref's
// commit, the branch the pin recorded; "" for none.
func executionStartedBranch(r *state.ExecutionRecord) string {
	if !publicationCommit(r.Request.GitRef) {
		return r.Request.GitRef
	}
	if r.Published != nil {
		return r.Published.Branch
	}
	return ""
}

// readReviewerFixedHead says whether head is a fast-forward descendant of
// assigned in ref's repository: ahead of it and behind it by nothing. Tests
// replace it.
var readReviewerFixedHead = func(ctx context.Context, ref, assigned, head string) (bool, error) {
	pr, ok := parseRef(ref)
	if !ok {
		return false, fmt.Errorf("invalid ref %s", ref)
	}
	raw, err := github.RunGH(ctx, "api", fmt.Sprintf("repos/%s/compare/%s...%s", pr.Repo, assigned, head))
	if err != nil {
		return false, err
	}
	var c struct {
		AheadBy  int `json:"ahead_by"`
		BehindBy int `json:"behind_by"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return false, err
	}
	return c.AheadBy > 0 && c.BehindBy == 0, nil
}

// reviewerFixedHead says whether head is the reviewer's own fix of round:
// the open pull request's current head, past the assigned head by a
// fast-forward. The approval of that head is the round's verdict.
func reviewerFixedHead(ctx context.Context, round *state.DeskReview, pull superloop.Pull, head string) bool {
	if head == "" || head == round.Head || pull.State != superloop.Open || pull.Head != head {
		return false
	}
	descends, err := readReviewerFixedHead(ctx, round.Ref, round.Head, head)
	return err == nil && descends
}
