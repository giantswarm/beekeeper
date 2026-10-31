package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const loopFillKey = "next capacity "

// fill repairs a shortfall without waiting for a done event. Each start
// checks live capacity again; the first shortfall also bounds this pass
// if a capacity read has not yet caught up with a start. Claims are per
// board item, so an interrupted start is reconciled before another start.
func (l *supervisorLoop) fill(ctx context.Context, st *state.State, claims map[string]loopClaim, taken []superloop.Taken) error {
	if l.dryRun || l.a.cfg.Supervisor.WorkerBrief == "" {
		return nil
	}
	attempts := map[string]int{}
	for _, t := range taken {
		if ref, ok := strings.CutPrefix(t.Key, loopFillKey); ok {
			ref, _, _ = strings.Cut(ref, " ")
			attempts[ref]++
		}
	}
	startedHere := map[string]bool{}
	for key, c := range claims {
		if !strings.HasPrefix(key, loopFillKey) {
			continue
		}
		act := superloop.Action{Kind: superloop.KindNext, Key: key}
		what, done, err := l.reconcile(st, act, c)
		if err != nil {
			return err
		}
		if done {
			if err := l.record(act, what); err != nil {
				return err
			}
			ref, _, _ := strings.Cut(strings.TrimPrefix(key, loopFillKey), " ")
			attempts[ref]++
			startedHere[ref] = true
		}
	}
	// A non-positive limit means the first fresh capacity read sets it.
	limit := 0
	for started := 0; ; started++ {
		out, code, err := beekeeperRun(ctx, l.as("capacity", "--json")...)
		if err != nil || code != 0 {
			return nil // a gate refusal is never a reason to start
		}
		var cap capacityView
		if err := json.Unmarshal([]byte(out), &cap); err != nil {
			l.sayOnce("fill capacity", "SUPERVISOR NEXT FAILED capacity --json: %v", err)
			return nil
		}
		shortfall := cap.Floor - len(cap.Busy)
		if cap.Room <= 0 || shortfall <= 0 {
			return nil
		}
		if started == 0 {
			limit = min(shortfall, cap.Room)
		}
		if started >= limit {
			return nil
		}
		next, err := l.nextItem(ctx)
		if err != nil {
			l.sayOnce("fill board", "SUPERVISOR NEXT FAILED board next: %v", err)
			return nil
		}
		if next == nil {
			return nil
		}
		if startedHere[next.Ref] {
			return nil // a stale board answer must not dispatch the same item twice
		}
		live, err := l.a.store.Read()
		if err != nil {
			return err
		}
		r, _ := parseRef(next.Ref)
		name := fmt.Sprintf("%s %d", repoName(r.Repo), r.N)
		if slices.ContainsFunc(live.Agents, func(ag state.Agent) bool { return ag.Name == name }) {
			return nil // the board may lag the worker's registration
		}
		act := superloop.Action{Kind: superloop.KindNext, Key: fmt.Sprintf("%s%s attempt %d", loopFillKey, next.Ref, attempts[next.Ref]+1)}
		what, err := l.startItem(ctx, act, *next)
		if err != nil {
			if !errors.Is(err, errLoopLater) {
				l.sayOnce(act.Key, "SUPERVISOR NEXT FAILED %s: %v", act.Key, err)
			}
			return nil
		}
		if err := l.record(act, what); err != nil {
			return fmt.Errorf("recording capacity refill: %w", err)
		}
		attempts[next.Ref]++
		startedHere[next.Ref] = true
	}
}
