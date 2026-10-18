package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/state"
)

// boardReleased is the event of a board item a closed decision held set
// back to the Status it had.
const boardReleased = "board.released"

// How a held decision closed, as its release says it.
const (
	closedAnswered  = "answered"
	closedDefaulted = "closed by its default"
	closedOvertaken = "closed as overtaken"
	closedDone      = "closed by hand"
)

// holdOn sets the board Status of ref to Blocked / Waiting for a decision
// that holds it and returns the hold to record on the note. An item an open
// note holds already keeps the Status that note recorded: the last release
// sets it back. An issue not on the board, or without a Status, is refused.
func (a *app) holdOn(ctx context.Context, notes []state.Note, ref string) (*state.ItemHold, error) {
	mv, err := a.boardClient().Hold(ctx, ref)
	if r := (*board.Refusal)(nil); errors.As(err, &r) {
		return nil, refused("--holds refused: %s", r.Reason)
	}
	if err != nil {
		return nil, fmt.Errorf("--holds %s: %w", ref, err)
	}
	h := &state.ItemHold{Ref: mv.Ref, Status: mv.To, Was: mv.From}
	if o := heldBy(notes, mv.Ref, 0); o != nil {
		h.Was = o.Held.Was
	}
	return h, nil
}

// heldBy is the open note among notes, other than note except, that holds
// the board item ref; nil for none.
func heldBy(notes []state.Note, ref string, except int) *state.Note {
	for i := range notes {
		if n := &notes[i]; n.ID != except && n.Held != nil && strings.EqualFold(n.Held.Ref, ref) {
			return n
		}
	}
	return nil
}

// releaseHolds sets the board items the closed notes held back to the
// Status each had (release) and returns what is said of each.
func (a *app) releaseHolds(ctx context.Context, by state.Party, closed []state.Note, how string) []string {
	var lines []string
	for i := range closed {
		if l := a.release(ctx, by, &closed[i], how); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// release sets the board item the closed note n held back to the Status it
// had, logs board.released and tells n's filer; "" when n holds nothing. An
// item another open note still holds stays held; one whose Status changed
// since the hold set it is left as it is, the filer told why.
func (a *app) release(ctx context.Context, by state.Party, n *state.Note, how string) string {
	h := n.Held
	if h == nil {
		return ""
	}
	line := ""
	if st, err := a.store.Read(); err == nil {
		if o := heldBy(st.Notes, h.Ref, n.ID); o != nil {
			line = fmt.Sprintf("HOLD KEPT: %s stays %s after note #%d %s: note #%d holds it still", h.Ref, h.Status, n.ID, how, o.ID)
		}
	}
	if line == "" {
		line = a.releaseItem(ctx, by, n, how)
	}
	if s := a.tellFiler(ctx, by, n, line); s != "" {
		_, _ = fmt.Fprintln(os.Stderr, s)
	}
	return line
}

// releaseItem sets n's held item back on the board and says what came of
// it.
func (a *app) releaseItem(ctx context.Context, by state.Party, n *state.Note, how string) string {
	h := n.Held
	mv, err := a.boardClient().Release(ctx, h.Ref, h.Status, h.Was)
	switch {
	case err != nil:
		return fmt.Sprintf("HOLD NOT RELEASED: %s stays %s after note #%d %s (%v): beekeeper board move %s %q sets it back",
			h.Ref, h.Status, n.ID, how, err, h.Ref, h.Was)
	case mv.Kept:
		return fmt.Sprintf("HOLD LEFT: %s is %s, set since note #%d held it %s: left as it is, not set back to %s",
			h.Ref, mv.To, n.ID, h.Status, h.Was)
	}
	line := fmt.Sprintf("BOARD RELEASED: %s back to %s, note #%d %s", h.Ref, mv.To, n.ID, how)
	if err := a.store.Update(func(*state.State) ([]state.Event, error) {
		return []state.Event{event(by, boardReleased, "%s %s → %s: note #%d %s", h.Ref, h.Status, mv.To, n.ID, how)}, nil
	}); err != nil {
		line += fmt.Sprintf(" (not logged: %v)", err)
	}
	return line
}
