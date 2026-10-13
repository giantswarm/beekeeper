package github

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestReadBaseChecks(t *testing.T) {
	answers := map[string]string{
		"repos/o/r/branches/release%2F1.x":       `{"commit":{"sha":"abc123"},"protection":{"required_status_checks":{"contexts":["go-build"],"checks":[{"context":"go-build"},{"context":"lint"}]}}}`,
		"repos/o/r/rules/branches/release%2F1.x": `[{"type":"pull_request"},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ci/circleci: go-test"}]}}]`,
		"repos/o/r/commits/abc123/check-runs?per_page=100": `{"name":"e2e-test","status":"completed","conclusion":"success","started_at":"2026-10-13T01:00:00Z"}
{"name":"e2e-test","status":"completed","conclusion":"failure","started_at":"2026-10-13T02:00:00Z"}
{"name":"lint","status":"in_progress","conclusion":null,"started_at":"2026-10-13T02:00:00Z"}
{"name":"docs","status":"completed","conclusion":"skipped","started_at":"2026-10-13T02:00:00Z"}`,
		"repos/o/r/commits/abc123/status?per_page=100": `{"statuses":[{"context":"ci/circleci: go-test","state":"failure"},{"context":"lint","state":"success"}]}`,
	}
	gh := func(_ context.Context, args ...string) ([]byte, error) {
		for _, a := range args {
			if out, ok := answers[a]; ok {
				return []byte(out), nil
			}
		}
		return nil, errors.New("unexpected gh " + strings.Join(args, " "))
	}
	b, err := ReadBaseChecks(context.Background(), gh, "o/r", "release/1.x")
	if err != nil {
		t.Fatal(err)
	}
	if b.Head != "abc123" || !slices.Equal(b.Required, []string{"ci/circleci: go-test", "go-build", "lint"}) {
		t.Errorf("head %q, required %q", b.Head, b.Required)
	}
	for name, want := range map[string]string{
		"e2e-test": CheckFail, "lint": CheckPending, "docs": CheckPass, "ci/circleci: go-test": CheckFail, "go-build": CheckMissing,
	} {
		if got := b.Outcome(name); got != want {
			t.Errorf("%s on the base head: %s, want %s", name, got, want)
		}
	}
}

func TestReadBaseChecksFailsWhenGitHubDoesNotAnswer(t *testing.T) {
	gh := func(context.Context, ...string) ([]byte, error) { return nil, errors.New("HTTP 502") }
	if _, err := ReadBaseChecks(context.Background(), gh, "o/r", "main"); err == nil {
		t.Error("no error without GitHub's answer")
	}
}
