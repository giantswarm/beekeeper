package cmd

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/state"
)

// boardHoldsView is the hand-over's board holds, read from GitHub with the
// read board next makes (board.Client.Read): every held board item with what
// holds it; Error why it was not read, never filled from the state. nil
// without a board.
type boardHoldsView struct {
	Held  []heldView `json:"held"`
	Error string     `json:"error,omitempty"`
	// epics are the epics in planning or past their go with their plans,
	// from the same read.
	epics []board.Epic
}

// heldView is a held board item and what holds it: its open blockers, its
// Status set after they closed, or the open decision that set it.
type heldView struct {
	Ref    string `json:"ref"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"`
	By     string `json:"by"`
}

// boardHoldsHandover reads the board holds for a hand-over; the open notes
// name the decisions that hold an item. A failed read is its error, not the
// hand-over's.
func (a *app) boardHoldsHandover(ctx context.Context, notes []state.Note) *boardHoldsView {
	if a.cfg.Board.Owner == "" || a.cfg.Board.Project == 0 {
		return nil
	}
	snap, err := a.boardClient().Read(ctx, a.now)
	if err != nil {
		return &boardHoldsView{Error: err.Error()}
	}
	bv := &boardHoldsView{epics: snap.Epics()}
	seen := map[string]bool{}
	add := func(h heldView) {
		if k := strings.ToLower(h.Ref); !seen[k] {
			seen[k] = true
			bv.Held = append(bv.Held, h)
		}
	}
	items := map[string]board.Item{}
	for _, it := range snap.Items {
		items[strings.ToLower(it.Ref)] = it
	}
	// A decision names its person: it comes first, whatever else holds the
	// item.
	for _, n := range notes {
		for _, h := range n.Holds() {
			it := items[strings.ToLower(h.Ref)]
			add(heldView{Ref: h.Ref, Title: it.Title, Status: cmp.Or(it.Status, h.Status),
				By: fmt.Sprintf("decision #%d for %s: %s (back to %s once it closes)", n.ID, noteWho(n), oneLine(n.Text), h.Was)})
		}
	}
	for _, it := range snap.Items {
		switch {
		case it.OpenBlockers > 0:
			add(heldView{Ref: it.Ref, Title: it.Title, Status: it.Status, By: it.HeldText()})
		case it.HeldByStatus():
			add(heldView{Ref: it.Ref, Title: it.Title, Status: it.Status, By: fmt.Sprintf("%s %s, set after its last blocker closed", board.HeldPrefix, it.Status)})
		}
	}
	return bv
}

// noteWho is the person a note is for.
func noteWho(n state.Note) string {
	if n.For == "" {
		return "nobody named"
	}
	return n.For
}

// printBoardHoldsHandover prints the held board items.
func (a *app) printBoardHoldsHandover(bv *boardHoldsView) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
	switch {
	case bv == nil:
		p("No board configured.")
		return
	case bv.Error != "":
		p("Not read: %s. `beekeeper board next` reads them.", bv.Error)
		return
	}
	if len(bv.Held) == 0 {
		p("Nothing on the board is held.")
		return
	}
	p("Held:")
	for _, h := range bv.Held {
		title := ""
		if h.Title != "" {
			title = " " + truncate(h.Title, 60)
		}
		p("- %s%s (%s): %s", h.Ref, title, h.Status, h.By)
	}
}

// promptBoard is the prompt's section of the board holds and the epics with
// their plans.
func (a *app) promptBoard(p printer, bv *boardHoldsView) {
	section(p, "Board holds", false, "")
	a.printBoardHoldsHandover(bv)
	p("")
	section(p, "Epics", false, "")
	a.printEpicsHandover(bv.epicsView())
	p("")
}

// epicsView is the epics of the same read, or why they were not read; nil
// without a board.
func (bv *boardHoldsView) epicsView() *epicsView {
	if bv == nil {
		return nil
	}
	return &epicsView{Epics: bv.epics, Error: bv.Error}
}
