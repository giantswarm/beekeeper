package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const loopFillKey = "next capacity "

type fillPause struct {
	signature string
	until     time.Time
}

// fillSignature changes when capacity or roster membership/work changes,
// independent of the order the readers return names in.
func fillSignature(cap *capacityView, st *state.State) string {
	busy := slices.Clone(cap.Busy)
	slices.Sort(busy)
	names := make([]string, 0, len(st.Agents))
	for _, ag := range st.Agents {
		park := ""
		if ag.Park != nil {
			park = ag.Park.On
		}
		names = append(names, fmt.Sprintf("%q/%q/%q/%q/%t", ag.Name, ag.Session, ag.Task, park, ag.Done))
	}
	slices.Sort(names)
	return fmt.Sprintf("%d/%d/%d/%q/%q", cap.Floor, cap.Ceiling, cap.Room, busy, names)
}

func (l *supervisorLoop) pauseFill(signature string) {
	l.fillPause = fillPause{signature: signature, until: l.a.now.Add(l.a.cfg.Supervisor.FillRetryEvery.Duration)}
}

func (l *supervisorLoop) fillCapacity(ctx context.Context) *capacityView {
	out, code, err := beekeeperRun(ctx, l.as("capacity", "--json")...)
	if err != nil || code != 0 {
		return nil
	}
	var cap capacityView
	if err := json.Unmarshal([]byte(out), &cap); err != nil {
		l.sayOnce("fill capacity", "SUPERVISOR NEXT FAILED capacity --json: %v", err)
		return nil
	}
	return &cap
}

func (l *supervisorLoop) previewFill(ctx context.Context) error {
	cap := l.fillCapacity(ctx)
	if cap == nil || cap.Floor <= len(cap.Busy) {
		return nil
	}
	room := cap.Floor - len(cap.Busy)
	if !l.placesRemotely() {
		room = min(room, cap.Room)
	}
	if room <= 0 {
		return nil
	}
	next, err := l.nextItem(ctx)
	if err != nil {
		return err
	}
	count, first := 0, "the board offers nothing"
	if next != nil {
		count, first = room, next.URL
	}
	_, err = fmt.Fprintf(l.a.out, "SUPERVISOR WOULD NEXT fill %d to capacity.floor (first: %s)\n", count, first)
	return err
}

// fill repairs a shortfall without waiting for a done event. Each start
// checks live capacity again; the first shortfall also bounds this pass
// if a capacity read has not yet caught up with a start. The local room
// bounds the pass only when no start can be placed on an execution host:
// with supervisor.hosts each start's placement judges the local machine
// and every host itself, so a full coordinator still fills onto hosts. Claims are per
// board item, so an interrupted start is reconciled before another start.
func (l *supervisorLoop) fill(ctx context.Context, st *state.State, claims map[string]loopClaim, taken []superloop.Taken) error {
	if l.a.cfg.Supervisor.WorkerBrief == "" {
		return nil
	}
	if l.dryRun {
		return l.previewFill(ctx)
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
		cap := l.fillCapacity(ctx)
		if cap == nil {
			return nil // a gate refusal is never a reason to start
		}
		live, err := l.a.store.Read()
		if err != nil {
			return err
		}
		signature := fillSignature(cap, live)
		if signature != l.fillPause.signature {
			l.fillPause = fillPause{}
		}
		shortfall := cap.Floor - len(cap.Busy)
		room := shortfall
		if !l.placesRemotely() {
			room = min(room, cap.Room)
		}
		if room <= 0 {
			return nil
		}
		if started == 0 {
			limit = room
		}
		if started >= limit {
			return nil
		}
		if l.fillPause.signature == signature && l.a.now.Before(l.fillPause.until) {
			return nil
		}
		next, err := l.nextItem(ctx)
		if err != nil {
			l.pauseFill(signature)
			l.sayOnce("fill board", "SUPERVISOR NEXT FAILED board next: %v", err)
			return nil
		}
		if next == nil {
			l.pauseFill(signature)
			return nil
		}
		if startedHere[next.Ref] {
			l.pauseFill(signature)
			return nil // a stale board answer must not dispatch the same item twice
		}
		r, _ := parseRef(next.Ref)
		name := fmt.Sprintf("%s %d", repoName(r.Repo), r.N)
		if slices.ContainsFunc(live.Agents, func(ag state.Agent) bool { return ag.Name == name }) {
			l.pauseFill(signature)
			return nil // the board may lag the worker's registration
		}
		act := superloop.Action{Kind: superloop.KindNext, Key: fmt.Sprintf("%s%s attempt %d", loopFillKey, next.Ref, attempts[next.Ref]+1)}
		what, err := l.startItem(ctx, act, *next)
		if err != nil {
			l.pauseFill(signature)
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
