package cmd

import (
	"fmt"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/state"
)

// An In Progress item whose only serve record is an ended owner's is
// offered by board next: a relieved guide run, a beekeeper-started agent
// off the roster (its saved review park too), a record the watch saw end.
// The current guide, a live agent and a role's successor still own theirs.
func TestEndedOwnersKeepNoItemServed(t *testing.T) {
	ref := func(n int) string { return fmt.Sprintf("o/r#%d", n) }
	listed := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	me := state.Party{Session: "me", Name: "Me"}
	guide := state.Party{Session: "g17", Name: "Guide run 17"}
	relieved := state.Party{Session: "g15", Name: "Guide run 15"}
	forgotten := state.Party{Session: "g12", Name: "Guide run 12"}
	successor := state.Party{Session: "g18", Name: "Guide run 18"}
	gone := state.Party{Session: "w1", Name: "beekeeper 2086"}
	live := state.Party{Session: "w2", Name: "beekeeper 2090"}
	ended := state.Party{Session: "w3", Name: "a person's session"}
	// Every CLI still shows: only the records decide.
	alive := func(p state.Party) bool { return p.Session != ended.Session }
	at := listed.Add(-time.Hour)
	st := &state.State{
		Starts: []state.Start{{Party: gone, At: at}, {Party: live, At: at}, {Party: successor, At: at}},
		Agents: []state.Agent{{Party: live, Task: "work o/r#6"}},
		Records: []state.Record{
			{Session: relieved, Issue: ref(1), At: at},
			{Session: forgotten, Issue: ref(2), At: at},
			{Session: gone, Issue: ref(3), At: at},
			{Session: ended, Issue: ref(4), At: at, Ended: at.Add(time.Minute)},
			{Session: guide, Issue: ref(5), At: at},
			{Session: live, Issue: ref(6), At: at},
			{Session: successor, Issue: ref(7), At: at},
		},
		ReviewParks: []state.ReviewPark{{On: ref(100), At: at, ReviewHead: "c0ffee", Agent: gone.Name, Party: gone, Serves: ref(8)}},
	}
	st.SetGuideRole(state.Role{Holder: &state.Supervisor{Party: guide, Since: at}, Relieved: []state.Relief{{Party: relieved, By: guide, At: at, Taken: at}}})
	var cands []board.Candidate
	for _, c := range boardCandidates(8) {
		c.Step, c.Status = stepInProgress, stepInProgress
		cands = append(cands, c)
	}
	sc := pickScope{me: me, alive: alive, listed: listed}
	owners := boardOwners(st, sc)
	for _, r := range []string{ref(1), ref(2), ref(3), ref(4), ref(8), ref(100)} {
		if who := owners[r]; who != "" {
			t.Errorf("%s is served by %s, an ended owner", r, who)
		}
	}
	for _, r := range []string{ref(5), ref(6), ref(7)} {
		if owners[r] == "" {
			t.Errorf("%s lost its live owner", r)
		}
	}
	res := nextFree(st, cands, sc)
	if res.Pick == nil || res.Pick.Ref != ref(1) {
		t.Fatalf("pick %+v, want o/r#1, the relieved run's In Progress item", res.Pick)
	}
}
