package cmd

import (
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
)

// Two teams' notes of one number, numbered before the shared note counter:
// note done closes the caller's team's and leaves the other team's open.
func TestNoteDoneClosesOnlyTheCallersTeamsNote(t *testing.T) {
	a, _ := noteApp(t)
	a.cfg.Identity.Team = ourTeam
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = []state.Note{
			{ID: 3, Kind: noteDecision, For: notePerson, Text: askedQ, By: state.Party{Name: agentOne, Team: piaTeam}},
			{ID: 3, Kind: noteDecision, For: notePerson, Text: askedQ, By: state.Party{Name: agentOne, Team: ourTeam}},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	noteCmdRun(t, a, "done", "3")
	st, _ := a.store.Read()
	if len(st.Notes) != 1 || st.Notes[0].By.Team != piaTeam {
		t.Fatalf("open notes %+v", st.Notes)
	}
}
