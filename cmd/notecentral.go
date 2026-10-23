package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
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
// its outcome through the person's mailbox. It returns the central note's
// number; an unreachable central instance is *central.Unreachable.
func (a *app) addCentral(ctx context.Context, me state.Party, n *state.Note, d noteDraft) (int, error) {
	args := map[string]any{paramText: n.Question, paramFor: n.For, paramKind: n.Kind,
		paramDue: n.Due.UTC().Format(time.RFC3339), paramDefault: d.Default, paramRefs: n.Refs}
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
	id, err := a.addCentral(ctx, me, n, d)
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
