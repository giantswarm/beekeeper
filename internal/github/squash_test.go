package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeGH answers the plain squash merge's gh calls: the pull request, its
// reviews and comments (reviews in turn, the last one repeats; "[]" when
// unset), the head commit's date, its checks in turn (the last one
// repeats), the merge and the branch deletion.
type fakeGH struct {
	pull     string
	reviews  []string
	comments string
	checks   []string
	merge    error
	calls    []string
}

func (f *fakeGH) run(_ context.Context, args ...string) ([]byte, error) {
	call := strings.Join(args, " ")
	f.calls = append(f.calls, call)
	switch {
	case call == "api user --jq .login":
		return []byte("teemow\n"), nil
	case strings.HasPrefix(call, "pr view"):
		return []byte(f.pull), nil
	case call == "api --paginate repos/teemow/lab/pulls/7/reviews":
		if len(f.reviews) == 0 {
			return []byte("[]"), nil
		}
		r := f.reviews[0]
		if len(f.reviews) > 1 {
			f.reviews = f.reviews[1:]
		}
		return []byte(r), nil
	case call == "api --paginate repos/teemow/lab/pulls/7/comments":
		return []byte("[]"), nil
	case call == "api --paginate repos/teemow/lab/issues/7/comments":
		if f.comments == "" {
			return []byte("[]"), nil
		}
		return []byte(f.comments), nil
	case strings.HasPrefix(call, "api repos/teemow/lab/commits/"):
		return []byte(headAt + "\n"), nil
	case strings.HasPrefix(call, "pr checks"):
		c := f.checks[0]
		if len(f.checks) > 1 {
			f.checks = f.checks[1:]
		}
		if c == "none" {
			return nil, errors.New("gh pr checks: exit status 1: no checks reported on the 'x' branch")
		}
		return []byte(c), nil
	case strings.HasPrefix(call, "api -X PUT"):
		if f.merge != nil {
			return nil, f.merge
		}
		return []byte(`{"sha":"abcdef1234567","merged":true}`), nil
	case strings.HasPrefix(call, "api -X DELETE"):
		return nil, nil
	}
	return nil, errors.New("unexpected gh " + call)
}

// pendingCI is a head whose check runs.
const pendingCI = `[{"name":"ci","bucket":"pending"}]`

// headAt is the head commit's committer date.
const headAt = "2026-10-11T10:00:00Z"

const openPull = `{"state":"OPEN","isDraft":false,"mergeable":"MERGEABLE","headRefOid":"1234567890","headRefName":"fix","title":"fix: it","author":{"login":"teemow","is_bot":false}}`

func squash(f *fakeGH) SquashDoc {
	now := time.Unix(0, 0)
	return Squash{GH: f.run, Repo: "teemow/lab", Number: 7, Timeout: time.Minute, Poll: 30 * time.Second,
		Now: func() time.Time { return now }, Sleep: func(d time.Duration) { now = now.Add(d) }}.Run(context.Background())
}

// A green head is squash-merged with its title and number as the subject,
// the judged head as the expected one, and its branch deleted.
func TestSquashMergesAGreenHead(t *testing.T) {
	f := &fakeGH{pull: openPull, checks: []string{pendingCI, `[{"name":"ci","bucket":"pass"}]`}}
	doc := squash(f)
	if doc.ExitCode != SquashMerged || doc.MergeCommitSha != "abcdef1234567" || doc.Release != nil || doc.UnansweredReviews == nil {
		t.Fatalf("doc %+v", doc)
	}
	joined := strings.Join(f.calls, "\n")
	for _, want := range []string{"sha=1234567890", "commit_title=fix: it (#7)", "merge_method=squash", "api -X DELETE repos/teemow/lab/git/refs/heads/fix"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in the calls:\n%s", want, joined)
		}
	}
}

// What devctl refuses before its wait, the plain route refuses with the
// same codes, and it merges nothing on red or a timeout.
func TestSquashRefusesAndWaits(t *testing.T) {
	for name, tc := range map[string]struct {
		pull, checks string
		merge        error
		want         int
	}{
		"merged":    {pull: strings.Replace(openPull, "OPEN", "MERGED", 1), want: SquashNotApplicable},
		"draft":     {pull: strings.Replace(openPull, `"isDraft":false`, `"isDraft":true`, 1), want: SquashNotApplicable},
		"conflict":  {pull: strings.Replace(openPull, "MERGEABLE", "CONFLICTING", 1), want: SquashNotApplicable},
		"a person":  {pull: strings.Replace(openPull, `"login":"teemow"`, `"login":"someone"`, 1), want: SquashRefused},
		"a bot":     {pull: strings.Replace(openPull, `"login":"teemow","is_bot":false`, `"login":"renovate","is_bot":true`, 1), checks: `[]`, want: SquashMerged},
		"red":       {pull: openPull, checks: `[{"name":"ci","bucket":"fail"}]`, want: SquashRed},
		"timeout":   {pull: openPull, checks: pendingCI, want: SquashTimeout},
		"no checks": {pull: openPull, checks: "none", want: SquashMerged},
		"declined":  {pull: openPull, checks: `[]`, merge: errors.New("gh api: HTTP 405: blocked"), want: SquashNotApplicable},
		"broken":    {pull: openPull, checks: `[]`, merge: errors.New("gh api: dial tcp: timeout"), want: SquashTooling},
	} {
		f := &fakeGH{pull: tc.pull, checks: []string{tc.checks}, merge: tc.merge}
		if doc := squash(f); doc.ExitCode != tc.want || doc.Verdict == "" || doc.Reason == "" {
			t.Errorf("%s: %+v, want exit %d", name, doc, tc.want)
		}
		if tc.want != SquashMerged && tc.merge == nil && strings.Contains(strings.Join(f.calls, "\n"), "-X PUT") {
			t.Errorf("%s: merged anyway", name)
		}
	}
}

// review is one review by login in state, on commit, submitted at.
func review(login, state, commit, at string) string {
	return fmt.Sprintf(`{"user":{"login":%q,"id":1,"type":"User"},"state":%q,"commit_id":%q,"submitted_at":%q,"html_url":"https://github.com/teemow/lab/pull/7#r"}`, login, state, commit, at)
}

// A reviewer's unanswered feedback refuses the merge (5) with devctl's
// reason, before the wait or when it lands while the checks run, and the
// document lists it; answered, approved, older than the head or a bot's,
// it holds nothing, and the document says so with an empty list.
func TestSquashRefusesAnUnansweredReview(t *testing.T) {
	const head = "1234567890"
	for name, tc := range map[string]struct {
		reviews  []string
		comments string
		want     int
		open     int
	}{
		"COMMENTED on the head":            {reviews: []string{`[` + review("pat", "COMMENTED", head, "2026-10-11T09:00:00Z") + `]`}, want: SquashRefused, open: 1},
		"changes requested after the head": {reviews: []string{`[` + review("pat", "CHANGES_REQUESTED", "old", "2026-10-11T11:00:00Z") + `]`}, want: SquashRefused, open: 1},
		"a comment after the head":         {comments: `[{"user":{"login":"pat","type":"User"},"created_at":"2026-10-11T11:00:00Z","html_url":"u"}]`, want: SquashRefused, open: 1},
		"landed while the checks ran":      {reviews: []string{`[]`, `[` + review("pat", "COMMENTED", head, "2026-10-11T11:00:00Z") + `]`}, want: SquashRefused, open: 1},
		"answered by the author": {reviews: []string{`[` + review("pat", "COMMENTED", head, "2026-10-11T11:00:00Z") + `]`},
			comments: `[{"user":{"login":"teemow","type":"User"},"created_at":"2026-10-11T12:00:00Z","html_url":"u"}]`, want: SquashMerged},
		"approved since":      {reviews: []string{`[` + review("pat", "COMMENTED", head, "2026-10-11T11:00:00Z") + `,` + review("pat", "APPROVED", head, "2026-10-11T12:00:00Z") + `]`}, want: SquashMerged},
		"older than the head": {reviews: []string{`[` + review("pat", "COMMENTED", "old", "2026-10-11T09:00:00Z") + `]`}, want: SquashMerged},
		"a bot's":             {comments: `[{"user":{"login":"renovate[bot]","type":"Bot"},"created_at":"2026-10-11T11:00:00Z","html_url":"u"}]`, want: SquashMerged},
		"no feedback":         {want: SquashMerged},
	} {
		f := &fakeGH{pull: openPull, reviews: tc.reviews, comments: tc.comments, checks: []string{pendingCI, `[]`}}
		doc := squash(f)
		if doc.ExitCode != tc.want || doc.UnansweredReviews == nil || len(doc.UnansweredReviews) != tc.open {
			t.Errorf("%s: %+v, want exit %d with %d unanswered", name, doc, tc.want, tc.open)
		}
		if tc.want == SquashRefused {
			prefix := "unanswered review: 1 item(s)"
			if doc.UnansweredReviews[0].State == "CHANGES_REQUESTED" {
				prefix = "changes requested: review CHANGES_REQUESTED by pat"
			}
			if !strings.HasPrefix(doc.Reason, prefix) || !strings.Contains(doc.Reason, "by pat") {
				t.Errorf("%s: reason %q", name, doc.Reason)
			}
			if strings.Contains(strings.Join(f.calls, "\n"), "-X PUT") {
				t.Errorf("%s: merged anyway", name)
			}
		}
		raw, _ := json.Marshal(doc)
		if strings.Contains(string(raw), `"unansweredReviews":null`) {
			t.Errorf("%s: the document carries no review state: %s", name, raw)
		}
	}
}

// A refusal before the wait still carries the review state it read.
func TestSquashRefusalCarriesTheReviewState(t *testing.T) {
	f := &fakeGH{pull: strings.Replace(openPull, `"isDraft":false`, `"isDraft":true`, 1)}
	if doc := squash(f); doc.ExitCode != SquashNotApplicable || doc.UnansweredReviews == nil {
		t.Errorf("a draft: %+v", doc)
	}
}

// Every page gh --paginate prints is read.
func TestListAllReadsEveryPage(t *testing.T) {
	gh := func(context.Context, ...string) ([]byte, error) {
		return []byte(`[{"state":"A"}]` + "\n" + `[{"state":"B"},{"state":"C"}]`), nil
	}
	rs, err := listAll[ghReview](context.Background(), gh, "p")
	if err != nil || len(rs) != 3 || rs[2].State != "C" {
		t.Errorf("%+v, %v", rs, err)
	}
}
