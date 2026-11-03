package cmd

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A ranked epic with free work and nobody serving it is unstaffed; one a
// session serves through an item under it (a candidate, or an item no
// candidate holds ranked under it), a held one and one without free work
// are not. The line names its rank, URL, title and next item.
func TestUnstaffedEpics(t *testing.T) {
	listed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	worker := state.Party{Session: "s1", Name: workerOne}
	// Epic n×100 of rank n has the items n×100+1 and up; 9 is unranked.
	ref := func(n int) string { return "e/p#" + strconv.Itoa(n) }
	epicOf := func(r string) string {
		for n := 100; n <= 500; n += 100 {
			for i := n; i < n+100; i++ {
				if r == ref(i) {
					return ref(n)
				}
			}
		}
		return ""
	}
	var cands []board.Candidate
	for _, n := range []int{101, 201, 202, 301, 401, 501, 9} {
		cands = append(cands, board.Candidate{Item: board.Item{Ref: ref(n)}, Step: stepUpNext, PriorityOf: epicOf(ref(n))})
	}
	cands[3].Skip = "held: sub-issue of " + ref(300) + ", blocked by " + ref(1)
	cands[5].Skip = "needs lease kind-1"
	epic := func(rank int, title string) board.RankedEpic {
		n := rank * 100
		return board.RankedEpic{Rank: rank, Ref: ref(n), URL: "https://github.com/e/p/issues/" + strconv.Itoa(n), Title: title}
	}
	held := epic(3, "Three")
	held.Held = "blocked by " + ref(1) + "; 1 of 1 blockers open"
	list := board.Priorities{epic(1, "One"), epic(2, "Two"), held, epic(4, "Four"), epic(5, "Five")}
	// Epic 1 is served through a candidate, epic 4 through an item no
	// candidate holds (an issue record ranked under it).
	st := &state.State{Records: []state.Record{
		{Session: worker, Issue: ref(101), At: listed.Add(-time.Hour)},
		{Session: worker, Issue: ref(410), At: listed.Add(-time.Hour)},
	}}
	sc := pickScope{me: state.Party{Session: "me"}, alive: func(p state.Party) bool { return p.Is(worker) }, listed: listed}
	got := unstaffedEpics(list, nextFree(st, cands, sc), epicOf)
	want := []unstaffedEpic{{Rank: 2, Ref: ref(200), URL: "https://github.com/e/p/issues/200", Title: "Two", Free: 2, Next: ref(201)}}
	if !slices.Equal(got, want) {
		t.Fatalf("unstaffed %+v, want %+v", got, want)
	}
	if l := got[0].line(); l != "EPIC UNSTAFFED priority 2 https://github.com/e/p/issues/200 Two: 2 free items under it and no session serves it, next e/p#201" {
		t.Errorf("line %q", l)
	}
	// Once a session serves an item under it, the epic has its worker.
	st.Records = append(st.Records, state.Record{Session: worker, Issue: ref(202), At: listed.Add(-time.Hour)})
	if got := unstaffedEpics(list, nextFree(st, cands, sc), epicOf); len(got) != 0 {
		t.Errorf("served, still unstaffed: %+v", got)
	}
}
