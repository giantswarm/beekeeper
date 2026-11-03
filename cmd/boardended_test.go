package cmd

import (
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
			{Session: relieved, Issue: "o/r#1", At: at},
			{Session: forgotten, Issue: "o/r#2", At: at},
			{Session: gone, Issue: "o/r#3", At: at},
			{Session: ended, Issue: "o/r#4", At: at, Ended: at.Add(time.Minute)},
			{Session: guide, Issue: "o/r#5", At: at},
			{Session: live, Issue: "o/r#6", At: at},
			{Session: successor, Issue: "o/r#7", At: at},
		},
		ReviewParks: []state.ReviewPark{{On: "o/r#100", At: at, ReviewHead: "abc", Agent: gone.Name, Party: gone, Serves: "o/r#8"}},
	}
	st.SetGuideRole(state.Role{Holder: &state.Supervisor{Party: guide, Since: at}, Relieved: []state.Relief{{Party: relieved, By: guide, At: at, Taken: at}}})
	var cands []board.Candidate
	for _, c := range boardCandidates(8) {
		c.Step, c.Status = stepInProgress, stepInProgress
		cands = append(cands, c)
	}
	sc := pickScope{me: me, alive: alive, listed: listed}
	owners := boardOwners(st, sc)
	for _, ref := range []string{"o/r#1", "o/r#2", "o/r#3", "o/r#4", "o/r#8", "o/r#100"} {
		if who := owners[ref]; who != "" {
			t.Errorf("%s is served by %s, an ended owner", ref, who)
		}
	}
	for _, ref := range []string{"o/r#5", "o/r#6", "o/r#7"} {
		if owners[ref] == "" {
			t.Errorf("%s lost its live owner", ref)
		}
	}
	res := nextFree(st, cands, sc)
	if res.Pick == nil || res.Pick.Ref != "o/r#1" {
		t.Fatalf("pick %+v, want o/r#1, the relieved run's In Progress item", res.Pick)
	}
}
