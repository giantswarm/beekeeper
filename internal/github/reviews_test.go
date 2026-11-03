package github

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// at is a time of the review tests' day.
func at(hm string) time.Time {
	t, err := time.Parse(time.RFC3339, "2026-10-21T"+hm+":00Z")
	if err != nil {
		panic(err)
	}
	return t
}

// patRequests is pat's standing request for changes as the tests list it.
const patRequests = "pat " + changesRequested

func rv(login, state, commit, hm string) ghReview {
	return ghReview{User: ghUser{Login: login, ID: 1, Type: "User"}, State: state, CommitID: commit, SubmittedAt: at(hm), HTMLURL: "https://github.com/o/r/pull/7#" + login + hm}
}

func cm(login, hm string) ghComment {
	return ghComment{User: ghUser{Login: login, ID: 1, Type: "User"}, CreatedAt: at(hm), HTMLURL: "https://github.com/o/r/pull/7#c" + login + hm}
}

// A reviewer's request for changes holds the merge while it is their latest
// verdict, whatever commits and replies came after it; their approval or its
// dismissal clears it, and the author's own feedback never holds anything.
func TestChangesRequestedHoldsUntilApprovedOrDismissed(t *testing.T) {
	for name, tc := range map[string]struct {
		f    feedback
		want []string
	}{
		// giantswarm/backstage#3184: marians requested changes at 07:33 on
		// 0c43ef0; the author replied and pushed three commits, the head
		// 24a3274 at 10:42, and the merge ran over the request.
		"the 3184 sequence": {f: feedback{headSHA: "24a3274", headAt: at("10:42"),
			reviews: []ghReview{rv("marians", "COMMENTED", "6fde8fd", "05:26"), rv("teemow", "COMMENTED", "0c43ef0", "06:40"),
				rv("marians", "CHANGES_REQUESTED", "0c43ef0", "07:33")},
			reviewComments: []ghComment{cm("marians", "05:26"), cm("teemow", "06:40")},
			comments:       []ghComment{cm("marians", "07:27"), cm("marians", "07:55"), cm("teemow", "09:42"), cm("teemow", "10:35")}},
			want: []string{"marians CHANGES_REQUESTED"}},
		"changes requested, then a comment of the same reviewer": {f: feedback{headSHA: "h", headAt: at("08:00"),
			reviews: []ghReview{rv("pat", "CHANGES_REQUESTED", "old", "07:00"), rv("pat", "COMMENTED", "old", "07:30")}},
			want: []string{patRequests}},
		"changes requested, then dismissed": {f: feedback{headSHA: "h", headAt: at("08:00"),
			reviews: []ghReview{rv("pat", "DISMISSED", "old", "07:00")}}},
		"changes requested, superseded by an approval": {f: feedback{headSHA: "h", headAt: at("08:00"),
			reviews: []ghReview{rv("pat", "CHANGES_REQUESTED", "old", "07:00"), rv("pat", "APPROVED", "h", "09:00")}}},
		"approved, then changes requested again": {f: feedback{headSHA: "h", headAt: at("08:00"),
			reviews: []ghReview{rv("pat", "APPROVED", "old", "06:00"), rv("pat", "CHANGES_REQUESTED", "old", "07:00")}},
			want: []string{patRequests}},
		"another reviewer's approval clears nothing": {f: feedback{headSHA: "h", headAt: at("08:00"),
			reviews: []ghReview{rv("pat", "CHANGES_REQUESTED", "old", "07:00"), rv("kim", "APPROVED", "h", "09:00")}},
			want: []string{patRequests}},
		"the author's own feedback": {f: feedback{headSHA: "h", headAt: at("08:00"),
			reviews:  []ghReview{rv("teemow", "CHANGES_REQUESTED", "h", "09:00")},
			comments: []ghComment{cm("teemow", "09:30")}}},
		"an unanswered comment after the head": {f: feedback{headSHA: "h", headAt: at("08:00"),
			comments: []ghComment{cm("pat", "09:00")}},
			want: []string{"pat "}},
	} {
		var got []string
		for _, item := range tc.f.unanswered("teemow", "teemow") {
			got = append(got, item.Login+" "+item.State)
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

// The refusal names the reviewer who requested changes and says what
// clears the request, apart from the unanswered feedback.
func TestReviewRefusalNamesTheReviewer(t *testing.T) {
	open := []UnansweredReview{
		{Kind: FeedbackReview, Login: "marians", State: "CHANGES_REQUESTED", URL: "u1"},
		{Kind: FeedbackComment, Login: "kim", URL: "u2"},
	}
	got := reviewRefusal(open, "teemow")
	for _, want := range []string{"changes requested: review CHANGES_REQUESTED by marians u1", "only the reviewer's approval or the review's dismissal clears it",
		"unanswered review: 1 item(s) newer than the head wait for an answer: comment by kim u2"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
}

// ReviewGuard reads the pull request, its author and head, as the gh login
// and refuses while a request for changes stands.
func TestReviewGuardReadsThePullRequest(t *testing.T) {
	gh := func(_ context.Context, args ...string) ([]byte, error) {
		path := args[len(args)-1]
		if len(args) > 2 && args[len(args)-2] == "--jq" {
			path = args[1]
		}
		switch path {
		case "user":
			return []byte("teemow\n"), nil
		case "repos/o/r/pulls/7":
			return []byte(`{"head":{"sha":"h"},"user":{"login":"teemow","type":"User"}}`), nil
		case "repos/o/r/pulls/7/reviews":
			return []byte(`[{"user":{"login":"marians","type":"User"},"state":"CHANGES_REQUESTED","commit_id":"old","submitted_at":"2026-10-21T07:33:46Z","html_url":"u"}]`), nil
		case "repos/o/r/pulls/7/comments", "repos/o/r/issues/7/comments":
			return []byte(`[]`), nil
		case "repos/o/r/commits/h":
			return []byte("2026-10-21T10:42:23Z\n"), nil
		}
		return nil, fmt.Errorf("unexpected gh %v", args)
	}
	why, open, err := ReviewGuard(context.Background(), gh, "o/r", 7)
	if err != nil || len(open) != 1 || !strings.Contains(why, "by marians") {
		t.Errorf("%q, %+v, %v", why, open, err)
	}
}
