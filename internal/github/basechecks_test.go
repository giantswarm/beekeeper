package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestReadBaseChecksReadsEveryStatusPage(t *testing.T) {
	for _, failContinuation := range []bool{false, true} {
		t.Run(fmt.Sprintf("continuationError=%t", failContinuation), func(t *testing.T) {
			continuationErr := errors.New("status continuation unavailable")
			type status struct {
				Context string `json:"context"`
				State   string `json:"state"`
			}
			first := struct {
				Statuses []status `json:"statuses"`
			}{}
			for i := 0; i < 100; i++ {
				first.Statuses = append(first.Statuses, status{fmt.Sprintf("ci-%d", i), "success"})
			}
			page, err := json.Marshal(first)
			if err != nil {
				t.Fatal(err)
			}
			gh := func(_ context.Context, args ...string) ([]byte, error) {
				for _, arg := range args {
					switch arg {
					case "repos/o/r/branches/main":
						return []byte(`{"commit":{"sha":"abc123"}}`), nil
					case "repos/o/r/rules/branches/main":
						return []byte(`[]`), nil
					case "repos/o/r/commits/abc123/check-runs?per_page=100":
						return []byte(`{"name":"ci-0","status":"completed","conclusion":"failure","started_at":"2026-10-13T02:00:00Z"}`), nil
					case "repos/o/r/commits/abc123/status?per_page=100":
						if !slices.Contains(args, "--paginate") {
							return page, nil
						}
						if failContinuation {
							return page, continuationErr
						}
						return append(append([]byte{}, page...), []byte(`
{"statuses":[{"context":"legacy-ci","state":"failure"},{"context":"pending-ci","state":"pending"}]}`)...), nil
					}
				}
				return nil, fmt.Errorf("unexpected gh arguments: %q", args)
			}
			b, err := ReadBaseChecks(context.Background(), gh, "o/r", "main")
			if failContinuation {
				if !errors.Is(err, continuationErr) {
					t.Fatalf("got error %v, want %v", err, continuationErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]string{"ci-0": CheckFail, "ci-99": CheckPass, "legacy-ci": CheckFail, "pending-ci": CheckPending} {
				if got := b.Outcome(name); got != want {
					t.Errorf("%s: got %s, want %s", name, got, want)
				}
			}
		})
	}
}
