package github

import (
	"context"
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
			case call == `api repos/o/plans/check-runs/42/annotations --jq [.[] | select(.title == "plan-stages") | .message]`:
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
		"none":            {other, CheckMissing, nil},
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
