package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
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
	if _, o := heldBy(notes, mv.Ref, 0); o != nil {
		h.Was = o.Was
	}
	return h, nil
}

// holdAll holds every ref for a decision (holdOn), the first as n.Held and
// the rest as n.AlsoHeld. When one is refused, those it already held are set
// back and the refusal returned: a decision holds all its --ref or none.
func (a *app) holdAll(ctx context.Context, notes []state.Note, n *state.Note, refs []string) error {
	var held []state.ItemHold
	for _, ref := range refs {
		h, err := a.holdOn(ctx, notes, ref)
		if err != nil {
			for _, u := range held {
				if _, o := heldBy(notes, u.Ref, 0); o == nil {
					_, _ = a.boardClient().Release(ctx, u.Ref, u.Status, u.Was)
				}
			}
			return err
		}
		held = append(held, *h)
	}
	if len(held) > 0 {
		n.Held, n.AlsoHeld = &held[0], slices.Clip(held[1:])
	}
	return nil
}

// heldBy is the open note among notes, other than note except, that holds
// the board item ref, and its hold of it; nil for none.
func heldBy(notes []state.Note, ref string, except int) (*state.Note, *state.ItemHold) {
	for i := range notes {
		if n := &notes[i]; n.ID != except {
			if h := n.HoldOf(ref); h != nil {
				return n, h
			}
		}
	}
	return nil, nil
}

// releaseHolds sets the board items the closed notes held back to the
// Status each had (release) and returns what is said of each.
func (a *app) releaseHolds(ctx context.Context, by state.Party, closed []state.Note, how string) []string {
	var lines []string
	for i := range closed {
		lines = append(lines, a.release(ctx, by, &closed[i], how)...)
	}
	return lines
}

// release sets every board item the closed note n held back to the Status
// it had, logs board.released for each and tells n's filer once; nothing
// when n holds nothing. An item another open note still holds stays held;
// one whose Status changed since the hold set it is left as it is, the
// filer told why.
func (a *app) release(ctx context.Context, by state.Party, n *state.Note, how string) []string {
	holds := n.Holds()
	if len(holds) == 0 {
		return nil
	}
	st, err := a.store.Read()
	lines := make([]string, 0, len(holds))
	for _, h := range holds {
		line := ""
		if err == nil {
			if o, _ := heldBy(st.Notes, h.Ref, n.ID); o != nil {
				line = fmt.Sprintf("HOLD KEPT: %s stays %s after note #%d %s: note #%d holds it still", h.Ref, h.Status, n.ID, how, o.ID)
			}
		}
		if line == "" {
			line = a.releaseItem(ctx, by, n.ID, h, how)
		}
		lines = append(lines, line)
	}
	if s := a.tellFiler(ctx, by, n, strings.Join(lines, "; ")); s != "" {
		_, _ = fmt.Fprintln(os.Stderr, s)
	}
	return lines
}

// releaseItem sets the board item h of note id back and says what came of
// it.
func (a *app) releaseItem(ctx context.Context, by state.Party, id int, h state.ItemHold, how string) string {
	mv, err := a.boardClient().Release(ctx, h.Ref, h.Status, h.Was)
	switch {
	case err != nil:
		return fmt.Sprintf("HOLD NOT RELEASED: %s stays %s after note #%d %s (%v): beekeeper board move %s %q sets it back",
			h.Ref, h.Status, id, how, err, h.Ref, h.Was)
	case mv.Kept:
		return fmt.Sprintf("HOLD LEFT: %s is %s, set since note #%d held it %s: left as it is, not set back to %s",
			h.Ref, mv.To, id, h.Status, h.Was)
	}
	line := fmt.Sprintf("BOARD RELEASED: %s back to %s, note #%d %s", h.Ref, mv.To, id, how)
	if err := a.store.Update(func(*state.State) ([]state.Event, error) {
		return []state.Event{event(by, boardReleased, "%s %s → %s: note #%d %s", h.Ref, h.Status, mv.To, id, how)}, nil
	}); err != nil {
		line += fmt.Sprintf(" (not logged: %v)", err)
	}
	return line
}
