package cmd

import (
	"github.com/giantswarm/beekeeper/internal/state"
	"testing"
)

const (
	verifyHead  = "abc1234def5678abc1234def5678abc1234def56"
	verifyEmail = "pat@example.com"
	verifyGuide = "Guide run 40"
	flagNote    = "--note"
)

// verifyNote files note 1 for the person on privateThread, its question
// naming the head it approves.
func verifyNote(t *testing.T, a *app) {
	t.Helper()
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = append(st.Notes, state.Note{ID: 1, For: notePerson, Kind: noteDecision, By: state.Party{Session: "w1", Name: noteFiler},
			Text: "Merge o/private#7 at abc1234?", Refs: []string{privateThread}})
		st.NextNote = 1
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// stubSeat puts the command in session (none: "") with or without a
// terminal on stdin for the test.
func stubSeat(t *testing.T, session string, terminal bool) {
	t.Helper()
	was := answerSeat
	answerSeat = func(*app) (string, bool) { return session, terminal }
	t.Cleanup(func() { answerSeat = was })
}
