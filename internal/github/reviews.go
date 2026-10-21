package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// The kinds of feedback the review guard reads, devctl pr merge's own.
const (
	FeedbackReview        = "review"
	FeedbackReviewComment = "review_comment"
	FeedbackComment       = "comment"
)

// changesRequested is the state of a review that requests changes.
const changesRequested = "CHANGES_REQUESTED"

// maxNamedFeedback bounds the items the refusal's reason names; the
// document lists all of them.
const maxNamedFeedback = 5

// automationAccounts are Giant Swarm's automation logins by account id, as
// devctl pr merge pins them: their feedback never holds a merge.
var automationAccounts = map[string]int64{
	"taylorbot":    25685558,
	"architectbot": 61872893,
}

// UnansweredReview is one piece of a reviewer's feedback the merge waits
// on, shaped as devctl pr merge's: a review, a comment on the diff or a
// comment on the conversation, newer than the head and not answered since.
type UnansweredReview struct {
	Kind  string    `json:"kind"`
	Login string    `json:"login"`
	State string    `json:"state,omitempty"`
	At    time.Time `json:"at"`
	URL   string    `json:"url"`
}

type ghUser struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

// automation says whether u is a bot, a GitHub App or one of the pinned
// automation accounts.
func (u ghUser) automation() bool {
	if u.Type == "Bot" || strings.HasSuffix(u.Login, "[bot]") {
		return true
	}
	id, known := automationAccounts[strings.ToLower(u.Login)]
	return known && id == u.ID
}

type ghReview struct {
	User        ghUser    `json:"user"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submitted_at"`
	CommitID    string    `json:"commit_id"`
	HTMLURL     string    `json:"html_url"`
}

type ghComment struct {
	User             ghUser    `json:"user"`
	CreatedAt        time.Time `json:"created_at"`
	OriginalCommitID string    `json:"original_commit_id"`
	HTMLURL          string    `json:"html_url"`
}

// feedback is the pull request's reviews and comments as the review guard
// reads them.
type feedback struct {
	reviews        []ghReview
	reviewComments []ghComment
	comments       []ghComment
	headSHA        string
	headAt         time.Time
}

// unansweredReviews reads the pull request's reviews, review comments,
// conversation comments and the head commit's date afresh and returns the
// feedback that holds the merge, never nil once read.
func (s Squash) unansweredReviews(ctx context.Context, head, author, caller string) ([]UnansweredReview, error) {
	return unansweredReviews(ctx, s.GH, s.Repo, s.Number, head, author, caller)
}

// unansweredReviews is the feedback on repo#n's head that holds its merge.
func unansweredReviews(ctx context.Context, gh GH, repo string, n int, head, author, caller string) ([]UnansweredReview, error) {
	f := feedback{headSHA: head}
	var err error
	if f.reviews, err = listAll[ghReview](ctx, gh, fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, n)); err != nil {
		return nil, err
	}
	if f.reviewComments, err = listAll[ghComment](ctx, gh, fmt.Sprintf("repos/%s/pulls/%d/comments", repo, n)); err != nil {
		return nil, err
	}
	if f.comments, err = listAll[ghComment](ctx, gh, fmt.Sprintf("repos/%s/issues/%d/comments", repo, n)); err != nil {
		return nil, err
	}
	out, err := gh(ctx, "api", fmt.Sprintf("repos/%s/commits/%s", repo, head), "--jq", ".commit.committer.date")
	if err != nil {
		return nil, err
	}
	if f.headAt, err = time.Parse(time.RFC3339, strings.TrimSpace(string(out))); err != nil {
		return nil, fmt.Errorf("the head %s's committer date: %w", short(head), err)
	}
	return f.unanswered(author, caller), nil
}

// ReviewGuard reads repo#n's reviews and comments afresh, as the gh login,
// and returns the refusal's reason while a reviewer's feedback holds the
// merge, "" with the empty list when nothing does: the gate's own read
// before devctl pr merge runs, whose guard drops a request for changes once
// a commit or a reply is newer.
func ReviewGuard(ctx context.Context, gh GH, repo string, n int) (string, []UnansweredReview, error) {
	me, err := gh(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return "", nil, err
	}
	out, err := gh(ctx, "api", fmt.Sprintf("repos/%s/pulls/%d", repo, n))
	if err != nil {
		return "", nil, err
	}
	var p struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		User ghUser `json:"user"`
	}
	if err := json.Unmarshal(out, &p); err != nil || p.Head.SHA == "" {
		return "", nil, fmt.Errorf("gh api repos/%s/pulls/%d: no head (%v)", repo, n, err)
	}
	caller := strings.TrimSpace(string(me))
	open, err := unansweredReviews(ctx, gh, repo, n, p.Head.SHA, p.User.Login, caller)
	if err != nil || len(open) == 0 {
		return "", open, err
	}
	return reviewRefusal(open, answerers(p.User.Login, p.User.automation(), caller)), open, nil
}

// listAll reads every page of a GitHub list endpoint; gh --paginate prints
// one JSON array per page.
func listAll[T any](ctx context.Context, gh GH, path string) ([]T, error) {
	raw, err := gh(ctx, "api", "--paginate", path)
	if err != nil {
		return nil, err
	}
	var all []T
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		var page []T
		if err := dec.Decode(&page); errors.Is(err, io.EOF) {
			return all, nil
		} else if err != nil {
			return nil, fmt.Errorf("gh api %s: %w", path, err)
		}
		all = append(all, page...)
	}
}

// unanswered is the feedback that holds the merge, oldest first. A
// reviewer's request for changes holds it while it is their latest verdict:
// newer commits and replies leave it standing, only their later approval
// or its dismissal (GitHub then reports it DISMISSED) clears it. The rest
// follows devctl pr merge's rule: its author is a person other than the
// pull request's author and the caller; it is newer than the head (on the
// head commit, or written after the head commit's date); it is a COMMENTED
// review or a comment. Anything the author or the caller wrote on the pull
// request after it answers it, and so does an APPROVED review of the same
// reviewer no older than it.
func (f feedback) unanswered(author, caller string) []UnansweredReview {
	answerer := func(u ghUser) bool {
		return strings.EqualFold(u.Login, author) || strings.EqualFold(u.Login, caller)
	}
	reviewer := func(u ghUser) bool { return !answerer(u) && !u.automation() }

	var lastAnswer time.Time
	answered := func(u ghUser, at time.Time) {
		if answerer(u) && at.After(lastAnswer) {
			lastAnswer = at
		}
	}
	approved := map[string]time.Time{}
	// verdict is each reviewer's latest review that decides: APPROVED,
	// CHANGES_REQUESTED or DISMISSED, as GitHub's own review state counts.
	verdict := map[string]ghReview{}

	var items []UnansweredReview
	for _, r := range f.reviews {
		answered(r.User, r.SubmittedAt)
		if !reviewer(r.User) {
			continue
		}
		login := strings.ToLower(r.User.Login)
		switch r.State {
		case "APPROVED", changesRequested, "DISMISSED":
			if v, ok := verdict[login]; !ok || !r.SubmittedAt.Before(v.SubmittedAt) {
				verdict[login] = r
			}
		}
		switch r.State {
		case "APPROVED":
			if r.SubmittedAt.After(approved[login]) {
				approved[login] = r.SubmittedAt
			}
		case "COMMENTED":
			if r.CommitID == f.headSHA || r.SubmittedAt.After(f.headAt) {
				items = append(items, UnansweredReview{Kind: FeedbackReview, Login: r.User.Login, State: r.State, At: r.SubmittedAt, URL: r.HTMLURL})
			}
		}
	}
	for _, c := range f.reviewComments {
		answered(c.User, c.CreatedAt)
		if reviewer(c.User) && (c.OriginalCommitID == f.headSHA || c.CreatedAt.After(f.headAt)) {
			items = append(items, UnansweredReview{Kind: FeedbackReviewComment, Login: c.User.Login, At: c.CreatedAt, URL: c.HTMLURL})
		}
	}
	for _, c := range f.comments {
		answered(c.User, c.CreatedAt)
		if reviewer(c.User) && c.CreatedAt.After(f.headAt) {
			items = append(items, UnansweredReview{Kind: FeedbackComment, Login: c.User.Login, At: c.CreatedAt, URL: c.HTMLURL})
		}
	}

	open := []UnansweredReview{}
	for _, item := range items {
		if lastAnswer.After(item.At) {
			continue
		}
		if approval, ok := approved[strings.ToLower(item.Login)]; ok && !approval.Before(item.At) {
			continue
		}
		open = append(open, item)
	}
	for _, r := range verdict {
		if r.State == changesRequested {
			open = append(open, UnansweredReview{Kind: FeedbackReview, Login: r.User.Login, State: r.State, At: r.SubmittedAt, URL: r.HTMLURL})
		}
	}
	slices.SortStableFunc(open, func(a, b UnansweredReview) int { return a.At.Compare(b.At) })
	return open
}

// answerers names who answers feedback: the author and the caller, or the
// caller alone on a bot's pull request, which cannot reply.
func answerers(author string, bot bool, caller string) string {
	if bot || strings.EqualFold(author, caller) {
		return caller
	}
	return author + " or " + caller
}

// reviewRefusal is the reason of the review refusal (exit 5): a reviewer's
// standing request for changes first, then devctl pr merge's words for the
// unanswered feedback: how many items wait, the first few by kind, reviewer
// and link, and what clears them.
func reviewRefusal(open []UnansweredReview, answerers string) string {
	var changes, rest []UnansweredReview
	for _, item := range open {
		if item.State == changesRequested {
			changes = append(changes, item)
		} else {
			rest = append(rest, item)
		}
	}
	var parts []string
	if len(changes) > 0 {
		parts = append(parts, fmt.Sprintf("changes requested: %s; a new commit or a reply leaves it standing, only the reviewer's approval or the review's dismissal clears it",
			named(changes)))
	}
	if len(rest) > 0 {
		parts = append(parts, fmt.Sprintf("unanswered review: %d item(s) newer than the head wait for an answer: %s; a reply from %s on the pull request, a new commit or the reviewer's approval clears them",
			len(rest), named(rest), answerers))
	}
	return strings.Join(parts, "; ")
}

// named lists the first few items by kind, reviewer and link.
func named(items []UnansweredReview) string {
	var out []string
	for _, item := range items[:min(len(items), maxNamedFeedback)] {
		what := strings.ReplaceAll(item.Kind, "_", " ")
		if item.State != "" {
			what += " " + item.State
		}
		out = append(out, fmt.Sprintf("%s by %s %s", what, item.Login, item.URL))
	}
	if len(items) > maxNamedFeedback {
		out = append(out, fmt.Sprintf("and %d more", len(items)-maxNamedFeedback))
	}
	return strings.Join(out, ", ")
}
