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

func TestPullCheckReadsAllAnnotations(t *testing.T) {
	const name = "plan-stages"
	const invalid = "plan: PRD has no problem statement"
	type annotation struct {
		Title   string `json:"title"`
		Message string `json:"message"`
	}
	var first []annotation
	for i := range 30 {
		first = append(first, annotation{name, fmt.Sprintf("plan%d: contrarian missing: no output", i)})
	}
	last := []annotation{{"runner", "Process completed with exit code 1"}, {name, invalid}}
	for _, tc := range []struct {
		name    string
		failure error
	}{
		{"case 0: all findings", nil},
		{"case 1: failed continuation", errors.New("annotation continuation unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(_ context.Context, args ...string) ([]byte, error) {
				if args[0] == "pr" {
					return []byte(`{"state":"OPEN","statusCheckRollup":[{"__typename":"CheckRun","name":"plan-stages","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"https://github.com/o/plans/actions/runs/1/job/42"}]}`), nil
				}
				if !slices.Contains(args, "repos/o/plans/check-runs/42/annotations") {
					t.Fatalf("unexpected call %q", args)
				}
				pages := [][]annotation{first}
				if slices.Contains(args, "--paginate") {
					if tc.failure != nil {
						return nil, tc.failure
					}
					pages = append(pages, last)
				}
				var out []byte
				for _, page := range pages {
					var value any = page
					if slices.Contains(args, "--jq") {
						messages := []string{}
						for _, a := range page {
							if a.Title == name {
								messages = append(messages, a.Message)
							}
						}
						value = messages
					}
					b, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					out = append(append(out, b...), '\n')
				}
				return out, nil
			}
			got, err := PullCheck(t.Context(), run, "o/plans", 7, name)
			if tc.failure != nil {
				if !errors.Is(err, tc.failure) {
					t.Fatalf("error = %v, want continuation failure", err)
				}
				return
			}
			if err != nil || got.Outcome != CheckFail || len(got.Annotations) != 31 || !slices.Contains(got.Annotations, invalid) {
				t.Fatalf("outcome %s, annotations %d, invalid PRD finding present %v, error %v", got.Outcome, len(got.Annotations), slices.Contains(got.Annotations, invalid), err)
			}
			if slices.ContainsFunc(got.Annotations, func(s string) bool { return strings.Contains(s, "Process completed") }) {
				t.Fatal("runner's annotation included")
			}
		})
	}
}
