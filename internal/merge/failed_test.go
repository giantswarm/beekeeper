package merge

import (
	"slices"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// failedRepo is the repository of the failed attempts under test.
const failedRepo = "o/r"

// A failed attempt replaces the pull request's earlier one, the attempts
// older than failedFor go, the next attempt drops it, and its lane shows it.
func TestFailedAttempts(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	st := &state.State{Failed: []state.Failed{
		{Repo: "o/old", PR: 1, Lane: "l", At: now.Add(-failedFor - time.Minute)},
		{Repo: failedRepo, PR: 7, Lane: "l", At: now.Add(-time.Hour), Reason: "first"},
		{Repo: "o/s", PR: 2, Lane: "other", At: now.Add(-time.Hour)},
	}}
	Fail(st, state.Failed{Repo: failedRepo, PR: 7, Lane: "l", At: now, Exit: 1, Reason: "second"})
	keys := func() []string {
		var k []string
		for _, f := range st.Failed {
			k = append(k, f.Key()+" "+f.Reason)
		}
		return k
	}
	if got, want := keys(), []string{"o/s#2 ", "o/r#7 second"}; !slices.Equal(got, want) {
		t.Errorf("after Fail: %q, want %q", got, want)
	}
	if q := Queue(st, "l"); len(q.Failed) != 1 || q.Failed[0].Reason != "second" {
		t.Errorf("lane l shows %+v", q.Failed)
	}
	Retry(st, failedRepo, 7)
	if got, want := keys(), []string{"o/s#2 "}; !slices.Equal(got, want) {
		t.Errorf("after Retry: %q, want %q", got, want)
	}
}

func TestFailedChecks(t *testing.T) {
	doc := `{"checks":[{"name":"a","conclusion":"failure"},{"name":"b","conclusion":"success"},{"name":"c","conclusion":"timed_out"},{"name":"a","conclusion":"failure"}],` +
		`"circleci":{"workflows":[{"name":"build","status":"failing"},{"name":"setup","status":"success"}]}}`
	if got, want := FailedChecks([]byte(doc)), []string{"a", "c", "circleci workflow build"}; !slices.Equal(got, want) {
		t.Errorf("FailedChecks = %q, want %q", got, want)
	}
	if got := FailedChecks([]byte("not json")); got != nil {
		t.Errorf("FailedChecks(not json) = %q", got)
	}
}
