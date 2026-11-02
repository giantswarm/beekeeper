package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
)

const withdrawalReason = "withdrawn by filer"

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
	noteCmdRun(t, a, "done", "3", "--reason", withdrawalReason)
	st, _ := a.store.Read()
	if len(st.Notes) != 1 || st.Notes[0].By.Team != piaTeam {
		t.Fatalf("open notes %+v", st.Notes)
	}
}

func TestNoteDoneRetainsReason(t *testing.T) {
	const reason = "Withdrawn: use the existing decision, \"keep\" 🐝\nno new approval needed"
	for _, jsonOutput := range []bool{false, true} {
		a, out := noteApp(t)
		n := state.Note{ID: 1, For: notePerson, Text: "Choose?\nWhy: a second line.", By: state.Party{Name: agentOne}}
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			st.Notes = []state.Note{n}
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"done", "1"}, {"done", "1", "--reason", " \n "}, {"done", "1", "--reason", reason, "--overtaken", "settled"}} {
			if _, err := noteCommand(a, args...); err == nil {
				t.Fatalf("accepted invalid withdrawal %q", args)
			}
			if st, _ := a.store.Read(); len(st.Notes) != 1 {
				t.Fatal("invalid withdrawal removed the note")
			}
		}
		noteCmdRun(t, a, "done", "1", "--reason", reason)
		if st, _ := a.store.Read(); len(st.Notes) != 0 {
			t.Fatal("withdrawn note stays open")
		}
		out.Reset()
		a.json = jsonOutput
		noteCmdRun(t, a, noteShowName, "1")
		if jsonOutput {
			var got struct {
				Reason, Text, For, Status string
				Closure                   *state.Event
			}
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Reason != reason || got.Text != n.Text || got.For != n.For || got.Status != "closed" || got.Closure == nil || got.Closure.Verb != noteDone {
				t.Fatalf("lost withdrawal: %+v", got)
			}
		} else if !strings.Contains(out.String(), "reason: "+reason) || !strings.Contains(out.String(), n.Text) {
			t.Fatalf("lost reason or question: %s", out)
		}
	}
}
