package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/state"
)

// centralNote says whether note add files n in the central instance: a
// decision of a machine with a central instance, neither holding board
// items nor folded into a note, which are the machine's.
func (a *app) centralNote(n *state.Note, holds bool, foldInto int) bool {
	return !a.central && a.cfg.Central.Enabled() && n.Kind == noteDecision && !holds && foldInto == 0
}

// addCentral files the checked decision n in the central instance
// (note_add), which puts it to its addressee in Slack and tells the filer
// its outcome through the person's mailbox, or, into not 0, folds it into
// the central note into. It returns the central note's number; an
// unreachable central instance is *central.Unreachable.
func (a *app) addCentral(ctx context.Context, me state.Party, n *state.Note, d noteDraft, into int) (int, error) {
	args := map[string]any{paramText: n.Question, paramFor: n.For, paramKind: n.Kind,
		paramDue: n.Due.UTC().Format(time.RFC3339), paramDefault: d.Default, paramRefs: n.Refs}
	if into != 0 {
		args[paramFoldInto] = into
	}
	for k, v := range map[string]string{paramStatusQuo: d.StatusQuo, paramWhy: d.Why, paramChecked: d.Checked} {
		if v != "" {
			args[k] = v
		}
	}
	if len(d.Options) > 0 {
		args[paramOptions] = d.Options
	}
	if d.Recommend != 0 {
		args[paramRecommend] = d.Recommend
	}
	var filed state.Note
	if _, err := a.callCentralRaw(ctx, me, "note_add", args, &filed); err != nil {
		return 0, err
	}
	if filed.ID <= 0 {
		return 0, fmt.Errorf("the central instance (%s) filed the note without its number", a.cfg.Central.Context)
	}
	return filed.ID, nil
}

// fileCentral files n centrally and logs it, with the plan stages its
// check found, on the machine. filed is false when the central instance is
// unreachable: the caller files n on the machine instead, as stderr says.
func (a *app) fileCentral(ctx context.Context, stderr io.Writer, me state.Party, n *state.Note, d noteDraft, stage planStage) (filed bool, err error) {
	id, err := a.addCentral(ctx, me, n, d, 0)
	if u := (*central.Unreachable)(nil); errors.As(err, &u) && u.Sent {
		return false, &exitError{code: ExitCentral, msg: u.Error() + ": the decision may be filed there, so it is not filed on this machine too; file it again once the central instance answers"}
	} else if u != nil {
		_, err = fmt.Fprintf(stderr, "%s: filed on this machine instead\n", u.Error())
		return false, err
	}
	if err != nil {
		return false, centralErr(err)
	}
	ctxName := a.cfg.Central.Context
	evs := append([]state.Event{event(me, "note.add", "#%d (central %s) %s", id, ctxName, n.Text)}, stage.events(me, id, n.For)...)
	for _, ev := range evs {
		_ = a.store.Log(ev)
	}
	_, err = fmt.Fprintln(a.out, "note #"+strconv.Itoa(id)+" (central "+ctxName+": put to "+n.For+" in Slack, its answer reaches this session)")
	return true, err
}

// foldCentral folds the checked decision n into the central note into
// (note_add's fold_into): that note's default and recommendation stand, and
// its answer reaches its filer. Nothing is filed on the machine instead.
func (a *app) foldCentral(ctx context.Context, me state.Party, n *state.Note, d noteDraft, into int) error {
	ref := a.cfg.Central.Context + "#" + strconv.Itoa(into)
	id, err := a.addCentral(ctx, me, n, d, into)
	if err != nil {
		return centralErr(err)
	}
	if id != into {
		return refused("the central instance (%s) filed this decision as its note #%d instead of folding it into #%d: it runs a beekeeper without note_add's fold_into; withdraw it with beekeeper note done %s#%d", a.cfg.Central.Context, id, into, a.cfg.Central.Context, id)
	}
	_ = a.store.Log(event(me, "note.folded", "into %s (--fold-into): %s", ref, n.Text))
	_, err = fmt.Fprintf(a.out, "note %s (folded: --fold-into: its default and recommendation stand)\n", ref)
	return err
}

// noteNamed settles s (<n>, #<n> or <context>#<n>) to the machine's or
// the central instance's note, as note done does: a bare number is the
// machine's while it has it open, and stays the machine's when neither has.
func (a *app) noteNamed(s string) (noteID, error) {
	named, err := a.parseNoteIDs([]string{s})
	if err != nil {
		return noteID{}, err
	}
	_, central, err := a.doneSources(named)
	id := named[0]
	id.Central, id.Bare = len(central) > 0, false
	return id, err
}

// parkCentralNote reads a park's On as a central note, <context>#<n>.
func parkCentralNote(on string) (ctx string, id int, ok bool) {
	ctx, num, found := strings.Cut(on, "#")
	n, err := strconv.Atoi(num)
	return ctx, n, found && err == nil && n > 0 && ctx != "" && !strings.ContainsAny(ctx, "/ ")
}

// parkNoteOn is the On of a park on the note s names: #<n> for the
// machine's, checked open as the park is set, <context>#<n> for an open
// note of the central instance's (note_list).
func (a *app) parkNoteOn(s string) (string, error) {
	id, err := a.noteNamed(s)
	if err != nil {
		return "", err
	}
	if !id.Central {
		return "#" + strconv.Itoa(id.ID), nil
	}
	notes, err := a.listCentralNotes()
	if err != nil {
		return "", err
	}
	ref := a.cfg.Central.Context + "#" + strconv.Itoa(id.ID)
	if !slices.ContainsFunc(notes, func(n state.Note) bool { return n.ID == id.ID }) {
		return "", refused("note %s is not open: its answer reached its filer as NOTE ANSWERED", ref)
	}
	return ref, nil
}

// The verbs a machine logs for what it did to a central note: its
// number is the central instance's, never one of the machine's notes.
const (
	noteCentralAnswer = "note.central-answer"
	noteCentralDone   = "note.central-done"
)

// centralNotesOn says whether the machine lists and acts on the central
// instance's notes beside its own: a machine with one, not the central
// instance itself.
func (a *app) centralNotesOn() bool { return !a.central && a.cfg.Central.Enabled() }

// centralNotes are the central instance's open notes (note_list), each
// marked with its context; none on a machine without one.
func (a *app) centralNotes(ctx context.Context, me state.Party) ([]state.Note, error) {
	if !a.centralNotesOn() {
		return nil, nil
	}
	var v struct {
		Notes []state.Note `json:"notes"`
	}
	if _, err := a.callCentral(ctx, me, "note_list", map[string]any{}, &v); err != nil {
		return nil, err
	}
	for i := range v.Notes {
		v.Notes[i].Central = a.cfg.Central.Context
	}
	return v.Notes, nil
}

// noteRef names n as the CLI takes it: #<n> for the machine's own note,
// <context>#<n> for a central instance's.
func noteRef(n *state.Note) string { return n.Central + "#" + strconv.Itoa(n.ID) }

// noteID is a note the CLI names: the machine's or, Central set, the
// central instance's; a bare number may be either.
type noteID struct {
	ID      int
	Central bool
	Bare    bool
}

// parseNoteIDs reads note ids: <n> or #<n>, either source, and
// <context>#<n>, the central instance's.
func (a *app) parseNoteIDs(args []string) ([]noteID, error) {
	ids := make([]noteID, 0, len(args))
	for _, s := range args {
		src, num, ok := strings.Cut(s, "#")
		if !ok {
			src, num = "", s
		}
		n, err := strconv.Atoi(num)
		if err != nil || n <= 0 {
			return nil, usageErr("%q is not a note id: <n>, or <context>#<n> for a central instance's", s)
		}
		switch {
		case src == "":
			ids = append(ids, noteID{ID: n, Bare: true})
		case a.centralNotesOn() && strings.EqualFold(src, a.cfg.Central.Context):
			ids = append(ids, noteID{ID: n, Central: true})
		case a.centralNotesOn():
			return nil, usageErr("%q names no central instance of this machine's: its notes are %s#<n>", s, a.cfg.Central.Context)
		default:
			return nil, usageErr("%q names a central instance, and this machine has none (central.context)", s)
		}
	}
	return ids, nil
}

// resolve settles a bare id: the machine's when local says it has it, the
// central instance's when only that one does. One both have is refused,
// naming both; one neither has stays the machine's, which refuses it.
func (a *app) resolve(id noteID, local bool, central []state.Note, cerr error) (noteID, error) {
	if !id.Bare || !a.centralNotesOn() {
		return id, nil
	}
	there := slices.ContainsFunc(central, func(n state.Note) bool { return n.ID == id.ID })
	switch {
	case local && there:
		return id, refused("note #%d is open on this machine and on the central instance: name it #%d or %s#%d", id.ID, id.ID, a.cfg.Central.Context, id.ID)
	case local:
		return id, nil
	case there:
		id.Central = true
	case cerr != nil:
		return id, cerr
	}
	return id, nil
}

// answerCentral answers central note id as the person (note_answer through
// muster): its addressee answers it, and the central instance tells its
// filer.
func (a *app) answerCentral(ctx context.Context, id, choice int, via, words string, post bool) error {
	ref := a.cfg.Central.Context + "#" + strconv.Itoa(id)
	switch {
	case post:
		return usageErr("--post is for this machine's notes: the central instance records %s's answer", ref)
	case via != viaCLI && via != viaSlack:
		return usageErr("--via %q is none of %s, %s", via, viaCLI, viaSlack)
	case choice == 0 && strings.TrimSpace(words) == "":
		return usageErr("an answer is --choice <n>, the person's words, or both")
	}
	me, err := a.caller()
	if err != nil {
		return err
	}
	args := map[string]any{paramNote: id, paramVia: via}
	if choice != 0 {
		args[paramChoice] = choice
	}
	if w := strings.TrimSpace(words); w != "" {
		args[paramText] = w
	}
	var v struct {
		Answer     string `json:"answer"`
		AnsweredBy string `json:"answeredBy"`
	}
	if _, err := a.callCentral(ctx, me, "note_answer", args, &v); err != nil {
		return err
	}
	_ = a.store.Log(event(me, noteCentralAnswer, "%s answered as %s: %s", ref, v.AnsweredBy, v.Answer))
	_, err = fmt.Fprintf(a.out, "note %s answered as %s: %s\n", ref, cmp.Or(v.AnsweredBy, me.Person), v.Answer)
	return err
}

// doneCentral withdraws central notes ids (note_done through muster): each
// its filer's, or its filer's team's supervisor role's, as the central
// instance decides. It stops at the first refused.
func (a *app) doneCentral(ctx context.Context, me state.Party, ids []int) error {
	for _, id := range ids {
		ref := a.cfg.Central.Context + "#" + strconv.Itoa(id)
		if _, err := a.callCentral(ctx, me, "note_done", map[string]any{paramNote: id}, nil); err != nil {
			return err
		}
		_ = a.store.Log(event(me, noteCentralDone, "%s withdrawn", ref))
		if _, err := fmt.Fprintf(a.out, "note %s done\n", ref); err != nil {
			return err
		}
	}
	return nil
}
