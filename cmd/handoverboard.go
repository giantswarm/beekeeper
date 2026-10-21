package cmd

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/state"
)

// prioritiesView is the hand-over's priorities and holds, read from GitHub
// with the read board next makes (board.Client.Read): the priority list,
// each ranked epic with its plan, and every held board item with what holds
// it; Error why it was not read, never filled from the state. nil without a
// board.
type prioritiesView struct {
	Ranked []rankedView `json:"ranked"`
	Held   []heldView   `json:"held"`
	Error  string       `json:"error,omitempty"`
	// epics are the epics in planning or past their go with their plans,
	// from the same read.
	epics []board.Epic
}

// rankedView is an epic of the priority list with its plan line, "" for an
// epic the epic gate does not watch.
type rankedView struct {
	board.RankedEpic
	Plan string `json:"plan,omitempty"`
}

// heldView is a held board item and what holds it: its open blockers, its
// Status set after they closed, or the open decision that set it.
type heldView struct {
	Ref    string `json:"ref"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"`
	By     string `json:"by"`
}

// prioritiesHandover reads the priorities and holds for a hand-over; the
// open notes name the decisions that hold an item. A failed read is its
// error, not the hand-over's.
func (a *app) prioritiesHandover(ctx context.Context, notes []state.Note) *prioritiesView {
	if a.cfg.Board.Owner == "" || a.cfg.Board.Project == 0 {
		return nil
	}
	snap, err := a.boardClient().Read(ctx, a.now)
	if err != nil {
		return &prioritiesView{Error: err.Error()}
	}
	pv := &prioritiesView{epics: snap.Epics()}
	for _, e := range snap.Priorities {
		r := rankedView{RankedEpic: e}
		if i := slices.IndexFunc(pv.epics, func(ep board.Epic) bool { return strings.EqualFold(ep.Ref, e.Ref) }); i >= 0 {
			r.Plan = pv.epics[i].PlanText()
		}
		pv.Ranked = append(pv.Ranked, r)
	}
	seen := map[string]bool{}
	add := func(h heldView) {
		if k := strings.ToLower(h.Ref); !seen[k] {
			seen[k] = true
			pv.Held = append(pv.Held, h)
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
	for _, e := range snap.Priorities {
		if e.Held != "" {
			add(heldView{Ref: e.Ref, Title: e.Title, Status: e.Status, By: e.Held})
		}
	}
	return pv
}

// noteWho is the person a note is for.
func noteWho(n state.Note) string {
	if n.For == "" {
		return "nobody named"
	}
	return n.For
}

// printPrioritiesHandover prints the priority list with the plans, then the held
// items.
func (a *app) printPrioritiesHandover(pv *prioritiesView) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
	switch {
	case pv == nil:
		p("No board configured.")
		return
	case pv.Error != "":
		p("Not read: %s. `beekeeper priority` and `beekeeper board next` read them.", pv.Error)
		return
	}
	if len(pv.Ranked) == 0 {
		p("No epic holds a rank (the board's %s field).", board.PriorityField)
	}
	for _, e := range pv.Ranked {
		line := fmt.Sprintf("%d. %s %s (%s)", e.Rank, e.Ref, truncate(e.Title, 60), e.Status)
		if e.Plan != "" {
			line += ": " + e.Plan
		}
		if e.Held != "" {
			line += "; " + e.Held
		}
		p("%s", line)
	}
	p("")
	if len(pv.Held) == 0 {
		p("Nothing on the board is held.")
		return
	}
	p("Held:")
	for _, h := range pv.Held {
		title := ""
		if h.Title != "" {
			title = " " + truncate(h.Title, 60)
		}
		p("- %s%s (%s): %s", h.Ref, title, h.Status, h.By)
	}
}

// promptPriorities is the prompt's section of the priorities and holds and
// the epics with their plans.
func (a *app) promptPriorities(p printer, pv *prioritiesView) {
	section(p, "Priorities and holds", false, "")
	a.printPrioritiesHandover(pv)
	p("")
	section(p, "Epics", false, "")
	a.printEpicsHandover(pv.epicsView())
	p("")
}

// epicsView is the epics of the same read, or why they were not read; nil
// without a board.
func (pv *prioritiesView) epicsView() *epicsView {
	if pv == nil {
		return nil
	}
	return &epicsView{Epics: pv.epics, Error: pv.Error}
}
