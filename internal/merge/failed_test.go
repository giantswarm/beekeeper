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

func TestQueueOrdersWaitingMergesByPriority(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	st := &state.State{Merges: []state.Merge{
		{Repo: failedRepo, PR: 1, Lane: failedRepo, By: state.Party{Session: "s-run"}, Phase: state.Running, Joined: now.Add(-3 * time.Minute), Started: now.Add(-3 * time.Minute)},
		{Repo: failedRepo, PR: 2, Lane: failedRepo, By: state.Party{Session: "s-unranked"}, Phase: state.Waiting, Joined: now.Add(-2 * time.Minute), Priority: 0},
		{Repo: failedRepo, PR: 3, Lane: failedRepo, By: state.Party{Session: "s-rank2"}, Phase: state.Waiting, Joined: now.Add(-time.Minute), Priority: 2},
		{Repo: failedRepo, PR: 4, Lane: failedRepo, By: state.Party{Session: "s-rank1"}, Phase: state.Waiting, Joined: now, Priority: 1},
	}}
	q := Queue(st, failedRepo)
	var prs []int
	for _, m := range q.Waiting {
		prs = append(prs, m.PR)
	}
	if want := []int{4, 3, 2}; !slices.Equal(prs, want) {
		t.Fatalf("waiting PR order = %v, want %v", prs, want)
	}
	// Once the running merge leaves, the rank-1 merge is first in the lane
	// and has no merge ahead of it.
	st.Merges = st.Merges[1:]
	q = Queue(st, failedRepo)
	if len(q.Waiting) == 0 || q.Waiting[0].PR != 4 {
		t.Fatalf("first waiting merge = %+v, want PR 4 (priority 1)", q.Waiting)
	}
	allow := func(state.Merge) bool { return true }
	alive := func(int) bool { return true }
	if _, behind := q.Ahead(failedRepo, 4, allow); behind {
		t.Fatal("rank-1 waiter PR 4 should not wait behind lower-priority merges")
	}
	if ahead, behind := q.Ahead(failedRepo, 3, allow); !behind || ahead.PR != 4 {
		t.Fatalf("rank-2 waiter PR 3 ahead = %+v, behind = %v; want PR 4", ahead, behind)
	}
	// Across two free lanes for the same party under a 1-slot cap, SessionTurn
	// gives the slot to the rank-1 merge ahead of an earlier rank-2 merge.
	sameParty := state.Party{Name: "supervisor-loop"}
	crossLane := &state.State{Merges: []state.Merge{
		{Repo: "o/r", PR: 3, Lane: "o/r", By: sameParty, Phase: state.Waiting, Joined: now.Add(-time.Minute), Priority: 2},
		{Repo: "o/s", PR: 4, Lane: "o/s", By: sameParty, Phase: state.Waiting, Joined: now, Priority: 1},
	}}
	_, _, okRank1 := SessionTurn(crossLane, crossLane.Merges[1], 1, allow, alive)
	_, first, okRank2 := SessionTurn(crossLane, crossLane.Merges[0], 1, allow, alive)
	if !okRank1 || okRank2 || first == nil || first.PR != 4 {
		t.Fatalf("SessionTurn did not pick rank-1 waiter: rank1=%v rank2=%v first=%+v", okRank1, okRank2, first)
	}
}
