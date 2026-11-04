package github

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestPullCheck(t *testing.T) {
	const job = "https://github.com/o/plans/actions/runs/1/job/42"
	run := func(rollup string) GH {
		return func(_ context.Context, args ...string) ([]byte, error) {
			call := strings.Join(args, " ")
			switch {
			case strings.HasPrefix(call, "pr view 7 --repo o/plans --json state,statusCheckRollup"):
				return []byte(`{"state":"OPEN","statusCheckRollup":[` + rollup + `]}`), nil
			case call == `api repos/o/plans/check-runs/42/annotations --paginate --jq [.[] | select(.title == "plan-stages") | .message]`:
				return []byte(`["p: contrarian missing: no p/contrarian"]`), nil
			}
			return nil, errors.New("unexpected gh " + call)
		}
	}
	other := `{"__typename":"StatusContext","context":"plan-stages","state":"SUCCESS"},{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS"}`
	old := `{"__typename":"CheckRun","name":"plan-stages","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-10-01T10:00:00Z","detailsUrl":"old"}`
	for name, c := range map[string]struct {
		rollup  string
		outcome string
		notes   []string
	}{
		"no check":        {other, CheckMissing, nil},
		"green":           {other + "," + old, CheckPass, nil},
		"pending, newest": {old + `,{"__typename":"CheckRun","name":"plan-stages","status":"IN_PROGRESS","startedAt":"2026-10-01T11:00:00Z","detailsUrl":"new"}`, CheckPending, nil},
		"red, newest":     {`{"__typename":"CheckRun","name":"plan-stages","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-10-01T11:00:00Z","detailsUrl":"` + job + `"},` + old, CheckFail, []string{"p: contrarian missing: no p/contrarian"}},
		"skipped is red":  {`{"__typename":"CheckRun","name":"plan-stages","status":"COMPLETED","conclusion":"SKIPPED","detailsUrl":"x"}`, CheckFail, nil},
	} {
		got, err := PullCheck(context.Background(), run(c.rollup), "o/plans", 7, "plan-stages")
		if err != nil || got.State != Open || got.Outcome != c.outcome || !slices.Equal(got.Annotations, c.notes) {
			t.Errorf("%s: %+v, %v; want %s %q", name, got, err, c.outcome, c.notes)
		}
	}
	fail := func(context.Context, ...string) ([]byte, error) { return nil, errors.New("HTTP 502") }
	if _, err := PullCheck(context.Background(), fail, "o/plans", 7, "plan-stages"); err == nil {
		t.Fatal("an unanswered read is an error")
	}
}

// gh emits a filtered JSON array for each page, even if a page has no
// annotations titled for this check. Later failures must discard earlier data.
func TestPullCheckReadsAllAnnotations(t *testing.T) {
	first := make([]string, 30)
	for i := range first {
		first[i] = "plan: contrarian missing: no plan/contrarian"
	}
	page, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	readErr := errors.New("annotation continuation unavailable")
	for _, tc := range []struct {
		name    string
		output  string
		err     error
		wantErr bool
	}{
		{name: "later PRD finding", output: string(page) + `
[]
["plan: PRD has no problem statement"]`},
		{name: "failed continuation", output: string(page), err: readErr, wantErr: true},
		{name: "malformed continuation", output: string(page) + `
[`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			run := func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				if args[0] == "pr" {
					return []byte(`{"state":"OPEN","statusCheckRollup":[{"__typename":"CheckRun","name":"plan-stages","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"https://github.com/o/plans/actions/runs/1/job/42"}]}`), nil
				}
				// The filter excludes runner annotations on every page, while
				// --paginate makes gh follow continuation links.
				want := []string{"api", "repos/o/plans/check-runs/42/annotations", "--paginate", "--jq", `[.[] | select(.title == "plan-stages") | .message]`}
				if !slices.Equal(args, want) {
					t.Fatalf("gh arguments = %q, want %q", args, want)
				}
				return []byte(tc.output), tc.err
			}
			got, err := PullCheck(context.Background(), run, "o/plans", 7, "plan-stages")
			if calls != 2 {
				t.Fatalf("got %d calls, want 2", calls)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatal("partial annotation read succeeded")
				}
				if tc.err != nil && !errors.Is(err, tc.err) {
					t.Fatalf("error = %v, want %v", err, tc.err)
				}
				if len(got.Annotations) != 0 {
					t.Fatalf("partial annotations returned: %q", got.Annotations)
				}
				return
			}
			want := append(slices.Clone(first), "plan: PRD has no problem statement")
			if err != nil || got.Outcome != CheckFail || !slices.Equal(got.Annotations, want) {
				t.Fatalf("PullCheck = %+v, %v; want annotations %q", got, err, want)
			}
		})
	}
}
