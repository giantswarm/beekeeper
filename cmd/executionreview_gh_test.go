package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// A reviewer's completion report names its review by the URL GitHub shows,
// https://github.com/<owner>/<repo>/pull/<n>#pullrequestreview-<id>. The
// check reads that review through gh at the pull request's own path,
// repos/<owner>/<repo>/pulls/<n>/reviews/<id>, and captures the result.
func TestExecutionReviewReportReadsReviewUnderItsPullRequest(t *testing.T) {
	const (
		url  = "https://github.com/o/r/pull/12#pullrequestreview-345"
		head = "0123456789abcdef0123456789abcdef01234567"
	)
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	answer := filepath.Join(dir, "review.json")
	writeFile(t, answer, `{"node_id":"PRR_345","html_url":"`+url+`","pull_request_url":"https://api.github.com/repos/o/r/pulls/12",`+
		`"body":"Review: approved\nHead reviewed: `+"`"+head+"`"+`","state":"COMMENTED","commit_id":"`+head+`",`+
		`"submitted_at":"`+time.Now().UTC().Format(time.RFC3339)+`","user":{"login":"reviewer-login"}}`)
	script := "#!/bin/sh\necho \"$*\" >>'" + calls + "'\ncase \"$*\" in\n" +
		"'api repos/o/r/pulls/12/reviews/345') cat '" + answer + "' ;;\n" +
		"*) echo 'gh: Not Found (HTTP 404)' >&2; exit 1 ;;\nesac\n"
	writeFile(t, filepath.Join(dir, "gh"), script)
	if err := os.Chmod(filepath.Join(dir, "gh"), 0o700); err != nil { //nolint:gosec // a fake gh
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	reviewer := state.Party{Name: "reviewer", Session: "reviewer-session"}
	st := &state.State{DeskReviews: []state.DeskReview{{
		ID: "round", Ref: "o/r#12", Head: head, Round: 1,
		Worker:     state.Party{Name: workerRole, Session: "assigned-worker"},
		Reviewer:   reviewer,
		AssignedAt: time.Now().Add(-time.Minute),
	}}}
	if err := (&app{}).captureExecutionReviewReport(context.Background(), st, reviewer, "Review: approved\n"+url, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(calls) //nolint:gosec // the fake gh's call log in the test's temp dir
	if err != nil {
		t.Fatal(err)
	}
	if want := "api repos/o/r/pulls/12/reviews/345"; strings.TrimSpace(string(got)) != want {
		t.Fatalf("gh called with %q, want %q", strings.TrimSpace(string(got)), want)
	}
	result := st.DeskReviews[0].Result
	if result == nil || result.ID != "PRR_345" || result.URL != url || result.Verdict != superloop.Approved {
		t.Fatalf("review result not captured: %+v", result)
	}
}

// A review GitHub files under another pull request is refused, even when its
// id was asked under the assigned one.
func TestReadDeskReviewRefusesAnotherPullRequestsReview(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gh"), "#!/bin/sh\necho '{\"node_id\":\"PRR_345\",\"pull_request_url\":\"https://api.github.com/repos/o/r/pulls/13\",\"state\":\"APPROVED\"}'\n")
	if err := os.Chmod(filepath.Join(dir, "gh"), 0o700); err != nil { //nolint:gosec // a fake gh
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := readDeskReview(context.Background(), "o/r", 12, "345"); err == nil {
		t.Fatal("a review of o/r#13 was accepted for o/r#12")
	}
}
