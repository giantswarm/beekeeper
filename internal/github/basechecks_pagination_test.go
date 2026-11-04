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

func TestReadBaseChecksReadsEveryStatusPage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failure   error
		truncated bool
	}{
		{"case 0: all status contexts", nil, false},
		{"case 1: failed continuation", errors.New("status continuation unavailable"), false},
		{"case 2: truncated continuation", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var statuses []map[string]string
			for i := range 100 {
				statuses = append(statuses, map[string]string{"context": fmt.Sprintf("ci-%d", i), "state": "success"})
			}
			run := func(_ context.Context, args ...string) ([]byte, error) {
				call := strings.Join(args, " ")
				switch {
				case strings.Contains(call, "/branches/main") && !strings.Contains(call, "/rules/"):
					return []byte(`{"commit":{"sha":"base"}}`), nil
				case strings.Contains(call, "/rules/"):
					return []byte(`[]`), nil
				case strings.Contains(call, "/check-runs?"):
					return []byte(`{"name":"ci-0","status":"completed","conclusion":"failure"}`), nil
				case strings.Contains(call, "/status?"):
					first, err := json.Marshal(map[string]any{"statuses": statuses})
					if err != nil {
						t.Fatal(err)
					}
					if !slices.Contains(args, "--paginate") {
						return first, nil
					}
					if tc.failure != nil {
						return nil, tc.failure
					}
					if tc.truncated {
						return append(first, []byte("\n"+`{"statuses":[`)...), nil
					}
					return append(first, []byte("\n"+`{"statuses":[{"context":"legacy-ci","state":"failure"}]}`)...), nil
				default:
					t.Fatalf("unexpected call %q", args)
					return nil, nil
				}
			}
			got, err := ReadBaseChecks(t.Context(), run, "o/r", "main")
			if tc.truncated {
				if err == nil {
					t.Fatal("incomplete status list reported as success")
				}
				return
			}
			if tc.failure != nil {
				if !errors.Is(err, tc.failure) {
					t.Fatalf("error = %v, want continuation failure", err)
				}
				return
			}
			if err != nil || got.Outcome("legacy-ci") != CheckFail || got.Outcome("ci-0") != CheckFail || got.Outcome("ci-99") != CheckPass {
				t.Fatalf("legacy-ci = %s, ci-0 = %s, ci-99 = %s, error %v", got.Outcome("legacy-ci"), got.Outcome("ci-0"), got.Outcome("ci-99"), err)
			}
		})
	}
}
