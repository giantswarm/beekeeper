package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func sameReviewSession(a, b state.Party) bool {
	return (a.Session != "" && a.Session == b.Session || a.HostSession != "" && a.HostSession == b.HostSession) && a.Is(b)
}

// Assignment is recorded only from a supervisor-loop start, never from a
// worker-selected name or a review's prose. Reconciliation is idempotent.
func (a *app) assignExecutionReview(ctx context.Context, act superloop.Action) error {
	return a.store.Update(func(st *state.State) ([]state.Event, error) {
		workerIndex := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == act.Agent })
		if workerIndex < 0 || st.Agents[workerIndex].Execution == nil {
			return nil, nil
		}
		for _, round := range st.DeskReviews {
			if round.Ref == act.Ref && round.Head == act.Head && round.Round == act.Round {
				return nil, nil
			}
		}
		name := superloop.ReviewerName(act.Ref, act.Round)
		var start *state.Start
		for i := range st.Starts {
			candidate := &st.Starts[i]
			if candidate.Name == name && candidate.By.Is(loopParty) && (start == nil || candidate.At.After(start.At)) {
				start = candidate
			}
		}
		if start == nil || start.At.IsZero() || start.Session == "" && start.HostSession == "" || sameReviewSession(start.Party, st.Agents[workerIndex].Party) {
			return nil, refused("independent reviewer start identity is unavailable")
		}
		st.DeskReviews = append(st.DeskReviews, state.DeskReview{ID: uuid.NewString(), Ref: act.Ref, Head: act.Head, Round: act.Round, Worker: st.Agents[workerIndex].Party, Reviewer: start.Party, AssignedAt: start.At})
		for _, report := range st.WorkerReports {
			if report.Finished && sameReviewSession(report.By, start.Party) {
				if err := a.captureExecutionReviewReport(ctx, st, report.By, report.Text, report.At, true); err != nil {
					return nil, err
				}
			}
		}
		return []state.Event{event(loopParty, "execution.review-assigned", "%s for %s at %s", name, act.URL, act.Head)}, nil
	})
}

func executionReviewRound(st *state.State, use execution.MergeUse) *state.DeskReview {
	ref := fmt.Sprintf("%s#%d", use.Repository, use.PR)
	for i := len(st.DeskReviews) - 1; i >= 0; i-- {
		if st.DeskReviews[i].Ref == ref {
			return &st.DeskReviews[i]
		}
	}
	return nil
}

// The public API supplies the stable node ID, author and exact commit for the
// review URL acknowledged by the authenticated reviewer session.
var readDeskReview = func(ctx context.Context, repository, id string) (superloop.Review, error) {
	raw, err := github.RunGH(ctx, "api", fmt.Sprintf("repos/%s/pulls/reviews/%s", repository, id))
	if err != nil {
		return superloop.Review{}, err
	}
	var r struct {
		ID     string    `json:"node_id"`
		URL    string    `json:"html_url"`
		Body   string    `json:"body"`
		State  string    `json:"state"`
		Commit string    `json:"commit_id"`
		At     time.Time `json:"submitted_at"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return superloop.Review{}, err
	}
	return superloop.Review{ID: r.ID, URL: r.URL, Author: r.User.Login, Head: superloop.ReviewedHead(r.Body, r.Commit), Commit: r.Commit, Verdict: superloop.Verdict(r.State, r.Body), Body: r.Body, At: r.At}, nil
}

// This runs in the coordinating state transaction, so its authenticated result
// survives report delivery/pruning and is rechecked immediately before use.
func (a *app) authenticateExecutionReview(st *state.State, use execution.MergeUse, pull superloop.Pull) error {
	round := executionReviewRound(st, use)
	_, workerIndex := executionRecord(st, use.Ref)
	if round == nil || round.Head != use.Head || workerIndex < 0 || !sameReviewSession(round.Worker, st.Starts[workerIndex].Party) || sameReviewSession(round.Worker, round.Reviewer) {
		return refused("no assigned independent review round for this execution and head")
	}
	result := round.Result
	if result == nil || !sameReviewSession(result.By, round.Reviewer) || result.Verdict != superloop.Approved {
		return refused("assigned independent reviewer has not approved this head")
	}
	currentIndex := slices.IndexFunc(pull.Reviews, func(review superloop.Review) bool { return review.ID == result.ID })
	if currentIndex < 0 {
		return refused("authenticated independent review no longer exists")
	}
	current := pull.Reviews[currentIndex]
	if current.Author != result.Author || current.Head != use.Head || current.Commit != use.Head || current.Verdict != superloop.Approved || execution.Digest([]byte(current.Body)) != result.BodyDigest {
		return refused("authenticated independent review changed; new supervisor approval required")
	}
	grant, err := execution.ReadMergeAuthorization(a.mergeAuthorityRoot(), use.ID)
	if err == nil && (grant.DeskReviewID != round.ID || grant.ReviewID != result.ID || grant.ReviewDigest != result.BodyDigest) {
		return refused("merge authorization belongs to a different independent review result")
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Completion reports already carry their authenticated caller in the roster
// and callback ledger. Capture the exact GitHub result in the same transaction
// before the generic worker-report delivery can prune it.
func (a *app) captureExecutionReviewReport(ctx context.Context, st *state.State, by state.Party, text string, at time.Time, finished bool) error {
	if !finished {
		return nil
	}
	for i := len(st.DeskReviews) - 1; i >= 0; i-- {
		round := &st.DeskReviews[i]
		if !sameReviewSession(round.Reviewer, by) || at.Before(round.AssignedAt) {
			continue
		}
		reportDigest := execution.Digest([]byte(text))
		if round.Result != nil && round.Result.ReportDigest == reportDigest && sameReviewSession(round.Result.By, by) {
			return nil
		}
		verdict := superloop.Verdict("COMMENTED", text)
		if verdict == "" {
			return refused("review completion report must begin with the exact Review: verdict line")
		}
		ref, ok := parseRef(round.Ref)
		if !ok {
			return refused("invalid assigned review reference")
		}
		pattern := regexp.MustCompile(regexp.QuoteMeta(fmt.Sprintf("https://github.com/%s/pull/%d#pullrequestreview-", ref.Repo, ref.N)) + `([0-9]+)\b`)
		links := pattern.FindAllStringSubmatch(text, -1)
		if len(links) != 1 {
			return refused("assigned reviewer's completion report must name one exact review URL")
		}
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		review, err := readDeskReview(ctx, ref.Repo, links[0][1])
		if err != nil {
			return err
		}
		if review.ID == "" || review.Author == "" || review.URL != links[0][0] || review.Head != round.Head || review.Commit != round.Head || review.At.Before(round.AssignedAt.Add(-30*time.Second)) || review.Verdict != verdict {
			return refused("reported desk review does not match its assigned head and round")
		}
		round.Result = &state.DeskReviewResult{ID: review.ID, URL: review.URL, Author: review.Author, Verdict: review.Verdict, BodyDigest: execution.Digest([]byte(review.Body)), ReportDigest: reportDigest, ReportedAt: at, By: by}
		return nil
	}
	return nil
}
