package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

// centralTarget says whether a hold on target lives in the central
// instance: a central lane's, or a repository's whose lane is central.
// "merges" and "github" are the machine's.
func (a *app) centralTarget(target string) bool {
	if name, ok := strings.CutPrefix(target, merge.LanePrefix); ok {
		l, known := a.cfg.LaneNamed(name)
		return known && a.cfg.CentralLane(l)
	}
	repo, _, ok := splitPR(target)
	return ok && a.cfg.CentralLane(a.cfg.LaneOf(repo))
}

// centralHolds are the central instance's holds.
func (a *app) centralHolds(me state.Party) ([]state.Hold, error) {
	var v struct {
		Holds []state.Hold `json:"holds"`
	}
	if _, err := a.callCentral(context.Background(), me, "hold_list", map[string]any{}, &v); err != nil {
		return nil, err
	}
	return v.Holds, nil
}

// setCentral holds a central target.
func (a *app) setCentral(target, reason, until, except string) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	args := map[string]any{paramTarget: target, paramReason: reason}
	if until != "" {
		args["until"] = until
	}
	if except != "" {
		args["except"] = except
	}
	text, err := a.callCentral(context.Background(), me, "hold_set", args, nil)
	if err != nil {
		return err
	}
	_ = a.store.Log(event(me, "hold.set", "%s (central %s): %s", target, a.cfg.Central.Context, reason))
	_, err = fmt.Fprintln(a.out, strings.TrimSpace(text))
	return err
}

// liftCentral lifts a central hold.
func (a *app) liftCentral(target string) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	text, err := a.callCentral(context.Background(), me, "hold_lift", map[string]any{paramTarget: target}, nil)
	if err != nil {
		return err
	}
	_ = a.store.Log(event(me, "hold.lift", "%s (central %s)", target, a.cfg.Central.Context))
	_, err = fmt.Fprintln(a.out, strings.TrimSpace(text))
	return err
}

// checkCentral refuses a central target the central instance holds.
func (a *app) checkCentral(target string) error {
	me, err := a.caller()
	if err != nil {
		me = state.Party{Name: a.cfg.Identity.Person}
	}
	holds, err := a.centralHolds(me)
	if err != nil {
		return err
	}
	st := &state.State{Holds: holds}
	h, ok := activeHold(st, a, target)
	if repo, pr, isRepo := splitPR(target); isRepo {
		h, ok = merge.Blocking(st, a.now, repo, pr, a.cfg.LaneOf(repo))
	}
	if ok {
		return refused("%s is held by %q until %s: %s", h.Target, partyName(h.By), untilText(a, h), h.Reason)
	}
	return nil
}
