package cmd

import (
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestDefaultEventsIdentifyTheNotesTeam(t *testing.T) {
	st := &state.State{Notes: []state.Note{
		{ID: 3, Kind: noteDecision, Due: time.Unix(1, 0), By: state.Party{Team: ourTeam}},
		{ID: 3, Kind: noteDecision, Due: time.Unix(1, 0), By: state.Party{Team: piaTeam}},
	}}
	_, events, _ := closeDefaulted(st, "", serveParty, time.Unix(2, 0))
	if len(events) != 2 || events[0].NoteTeam != ourTeam || events[1].NoteTeam != piaTeam {
		t.Fatalf("legacy numbers cannot identify the notes without their teams: %+v", events)
	}
}
