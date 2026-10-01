package cmd

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/state"
)

func boardCandidates(n int) []board.Candidate {
	out := make([]board.Candidate, n)
	for i := range out {
		out[i] = board.Candidate{Item: board.Item{Ref: fmt.Sprintf("o/r#%d", i+1)}, Step: "Up Next"}
	}
	return out
}

// idleRef is the item an agent reporting idle still has a record of.
const idleRef = "o/r#2"

func TestNextFreeSkipsOwnedItems(t *testing.T) {
	listed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	me := state.Party{Session: "me", Name: "Me"}
	running := state.Party{Session: "s1", Name: "Worker one"}
	idleAgent := state.Party{Session: "s2", Name: "Idle agent"}
	gone := state.Party{Session: "s3", Name: "Gone"}
	late := state.Party{Session: "s4", Name: "Started late"}
	alive := func(p state.Party) bool { return p.Is(running) || p.Is(idleAgent) || p.Is(me) }
	st := &state.State{
		Records: []state.Record{
			{Session: running, Issue: "O/R#1", At: listed.Add(-time.Hour)},
			{Session: idleAgent, Issue: idleRef, At: listed.Add(-time.Hour)},
			{Session: gone, Issue: "o/r#3", At: listed.Add(-time.Hour)},
			{Session: late, Issue: "o/r#4", At: listed.Add(time.Second)},
			{Session: running, Issue: "o/r#5", At: listed.Add(-time.Hour), Ended: listed},
			{Session: me, Issue: "o/r#6", At: listed.Add(-time.Hour)},
		},
		Agents: []state.Agent{
			{Party: idleAgent, LastTask: idleRef},
			{Party: state.Party{Session: "s5", Name: "Busy"}, Task: "https://github.com/o/r/issues/7 and o/r#8"},
			{Party: state.Party{Session: "s6", Name: "Finished"}, Task: "o/r#9", Done: true},
		},
		Notes: []state.Note{{ID: 12, For: "Pat", Text: "decide https://github.com/o/r/issues/10"}},
	}
	cands := boardCandidates(11)
	cands[10].Skip = "assigned to pat"
	res := nextFree(st, cands, me, alive, listed)
	if res.Pick == nil || res.Pick.Ref != idleRef || len(res.Skipped) != 1 || res.Skipped[0].Skip != `served by "Worker one"` {
		t.Errorf("an idle agent's record is no claim: pick %v, skipped %+v", res.Pick, res.Skipped)
	}
	// The ended record's, the gone session's and the finished agent's items
	// are free; once a running session serves them nothing is.
	st.Records = append(st.Records, state.Record{Session: running, Issue: idleRef}, state.Record{Session: running, Issue: "o/r#3"},
		state.Record{Session: running, Issue: "o/r#5"}, state.Record{Session: running, Issue: "o/r#9"})
	if res = nextFree(st, cands, me, alive, listed); res.Pick != nil {
		t.Errorf("all owned, picked %v", res.Pick)
	}
	var got []string
	for _, c := range res.Skipped {
		got = append(got, c.Ref+": "+c.Skip)
	}
	want := []string{
		`o/r#1: served by "Worker one"`,
		idleRef + `: served by "Worker one"`,
		`o/r#3: served by "Worker one"`,
		`o/r#4: served by "Started late"`,
		`o/r#5: served by "Worker one"`,
		`o/r#6: served by you`,
		`o/r#7: served by "Busy" (task)`,
		`o/r#8: served by "Busy" (task)`,
		`o/r#9: served by "Worker one"`,
		`o/r#10: note #12 (waits on Pat)`,
		`o/r#11: assigned to pat`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("skipped:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestNextFreeHoldsAnEpicsSubIssuesToItsOwners(t *testing.T) {
	listed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	me := state.Party{Session: "me", Name: "Me"}
	running := state.Party{Session: "s1", Name: "Worker one"}
	alive := func(p state.Party) bool { return p.Is(running) || p.Is(me) }
	st := &state.State{
		Records: []state.Record{
			{Session: running, Issue: "o/r#20", At: listed.Add(-time.Hour)},
			{Session: running, Issue: "o/r#21", At: listed.Add(-time.Hour)},
		},
		Notes: []state.Note{{ID: 704, For: "Supervisor", Pinned: true, Text: "skip the slices of https://github.com/o/r/issues/10"}},
	}
	sub := func(n int, epic string) board.Candidate {
		return board.Candidate{Item: board.Item{Ref: fmt.Sprintf("o/r#%d", n)}, Step: "In Progress", Epic: epic}
	}
	// A sub-issue's own record names it ahead of its epic's.
	cands := []board.Candidate{sub(11, "o/r#10"), sub(12, "O/R#10"), sub(21, "o/r#20"), sub(22, "o/r#20"), sub(31, "o/r#30")}
	res := nextFree(st, cands, me, alive, listed)
	var got []string
	for _, c := range res.Skipped {
		got = append(got, c.Ref+": "+c.Skip)
	}
	want := []string{
		`o/r#11: note #704 (waits on Supervisor), on epic o/r#10`,
		`o/r#12: note #704 (waits on Supervisor), on epic O/R#10`,
		`o/r#21: served by "Worker one"`,
		`o/r#22: served by "Worker one", on epic o/r#20`,
	}
	if !slices.Equal(got, want) || res.Pick == nil || res.Pick.Ref != "o/r#31" {
		t.Errorf("pick %v, skipped:\n%s\nwant o/r#31 after:\n%s", res.Pick, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestConcurrentClaimsNeverGetTheSameItem(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	cands := boardCandidates(workers - 2)
	listed := time.Now()
	alive := func(state.Party) bool { return true }
	var wg sync.WaitGroup
	picks := make([]string, workers)
	for i := range workers {
		wg.Go(func() {
			me := state.Party{Session: fmt.Sprintf("s%d", i), Name: fmt.Sprintf("Board pull %d", i)}
			res, err := claimNext(store, cands, me, alive, listed, "")
			if err != nil {
				t.Error(err)
			}
			if res.Pick != nil && res.Claimed {
				picks[i] = res.Pick.Ref
			}
		})
	}
	wg.Wait()
	got := slices.DeleteFunc(slices.Clone(picks), func(s string) bool { return s == "" })
	slices.Sort(got)
	if len(got) != len(cands) || len(slices.Compact(slices.Clone(got))) != len(got) {
		t.Errorf("claims %v: want each of the %d items once", picks, len(cands))
	}
	st, _ := store.Read()
	if len(st.Records) != len(cands) {
		t.Errorf("records %+v", st.Records)
	}
	// A second claim by a session takes the next item and replaces its
	// record: it serves one item.
	me := st.Records[0].Session
	res, err := claimNext(store, boardCandidates(workers-1), me, alive, listed, "the review")
	if err != nil || res.Pick == nil || res.Pick.Ref != fmt.Sprintf("o/r#%d", workers-1) {
		t.Fatalf("re-claim: %+v, %v", res, err)
	}
	st, _ = store.Read()
	mine := slices.DeleteFunc(slices.Clone(st.Records), func(r state.Record) bool { return !r.Session.Is(me) })
	if len(mine) != 1 || mine[0].Issue != res.Pick.Ref || mine[0].Waits != "the review" || len(st.Records) != len(cands) {
		t.Errorf("records after the re-claim %+v", st.Records)
	}
}

func TestClaimTakesNoSkippedItem(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cands := boardCandidates(1)
	cands[0].Skip = "created 2026-06-24: Backlog takes items created within 90 days"
	me := state.Party{Session: "s1", Name: "Board pull"}
	res, err := claimNext(store, cands, me, func(state.Party) bool { return true }, time.Now(), "")
	st, _ := store.Read()
	if err != nil || res.Pick != nil || res.Claimed || len(st.Records) != 0 || len(res.Skipped) != 1 {
		t.Errorf("claim over a skipped item: %+v, %v; records %+v", res, err, st.Records)
	}
}

func TestFindRecordTakesASessionOrTheIssueItServes(t *testing.T) {
	const own, shared, pull = "o/s#1", "o/s#2", "Pull two"
	records := []state.Record{
		{Session: state.Party{Session: "s1", Name: "Pull one"}, Issue: own},
		{Session: state.Party{Session: "s2", Name: pull}, Issue: shared},
		{Session: state.Party{Session: "s3", Name: "Review"}, Issue: shared},
	}
	for q, want := range map[string]int{"O/S#1": 0, pull: 1, "review": 2} {
		if i, err := findRecord(records, q); err != nil || i != want {
			t.Errorf("findRecord(%q) = %d, %v; want %d", q, i, err, want)
		}
	}
	for q, want := range map[string]string{"o/s#3": "no session record serves o/s#3", shared: shared + ` is served by "` + pull + `", "Review": name the session`} {
		if _, err := findRecord(records, q); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("findRecord(%q) = %v, want %q", q, err, want)
		}
	}
}
