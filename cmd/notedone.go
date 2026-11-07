package cmd

import (
	"fmt"

	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	noteStatusClosed = "closed"
	flagNoteReason   = "--reason"
)

// withdrawnEvent retains the reason and original note for file-backed history.
// Quoted fields keep multiline text and punctuation unambiguous.
func withdrawnEvent(by state.Party, n state.Note, reason string) state.Event {
	e := event(by, noteDone, "#%d withdrawn: %q (asked by %q, for %q: %q)", n.ID, reason, n.By.Name, n.For, n.Text)
	e.NoteSnapshot = &n
	return e
}

func withdrawnNote(e state.Event) *shownNote {
	if e.Verb != noteDone {
		return nil
	}
	var n state.Note
	var reason string
	if count, err := fmt.Sscanf(e.Detail, "#%d withdrawn: %q (asked by %q, for %q: %q)", &n.ID, &reason, &n.By.Name, &n.For, &n.Text); err != nil || count != 5 {
		return nil
	}
	if e.NoteSnapshot != nil {
		n = *e.NoteSnapshot
	}
	return &shownNote{Note: n, Status: noteStatusClosed, Reason: reason, Closure: &e}
}
