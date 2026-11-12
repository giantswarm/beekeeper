package loop

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// TakenFold is the loop's own history folded into what a pass plans from:
// steps taken, claims, finished stages, ownerless and failing steps. The
// history only grows, so a pass folds the events appended since the last
// one instead of parsing every supervisor.* event again.
type TakenFold struct {
	mu sync.Mutex
	// n events are folded; last is the n-th, to notice a history that is
	// not the one folded.
	n    int
	last state.Event

	taken       []superloop.Taken
	claims      map[string]LoopClaim
	stages      map[string]bool
	stageDetail map[string]string
	ownerless   map[string]time.Time
	failures    map[string]LoopFailure
	stuckFiled  map[string]time.Time
}

// foldedTaken is one pass's copy of the fold, its own to change.
type foldedTaken struct {
	Taken       []superloop.Taken
	Claims      map[string]LoopClaim
	Stages      map[string]bool
	stageDetail map[string]string
	ownerless   map[string]time.Time
	failures    map[string]LoopFailure
	stuckFiled  map[string]time.Time
}

func (f *TakenFold) reset() {
	f.n, f.last, f.taken = 0, state.Event{}, nil
	f.claims, f.stages, f.stageDetail = map[string]LoopClaim{}, map[string]bool{}, map[string]string{}
	f.ownerless, f.failures, f.stuckFiled = map[string]time.Time{}, map[string]LoopFailure{}, map[string]time.Time{}
}

func sameFoldedEvent(a, b state.Event) bool {
	return a.At.Equal(b.At) && a.By.Name == b.By.Name && a.Verb == b.Verb && a.Subject == b.Subject && a.Detail == b.Detail
}

// Fold takes in history, the loop's supervisor.* events oldest first, and
// returns a copy of the result. A history shorter than the one folded, or
// one that differs at its last folded event, is folded from the start.
func (f *TakenFold) Fold(history []state.Event) foldedTaken {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claims == nil || len(history) < f.n || f.n > 0 && !sameFoldedEvent(history[f.n-1], f.last) {
		f.reset()
	}
	for _, e := range history[f.n:] {
		f.add(e)
	}
	f.n = len(history)
	if f.n > 0 {
		f.last = history[f.n-1]
	}
	return foldedTaken{
		Taken:       slices.Clone(f.taken),
		Claims:      maps.Clone(f.claims),
		Stages:      maps.Clone(f.stages),
		stageDetail: maps.Clone(f.stageDetail),
		ownerless:   maps.Clone(f.ownerless),
		failures:    maps.Clone(f.failures),
		stuckFiled:  maps.Clone(f.stuckFiled),
	}
}

func (f *TakenFold) add(e state.Event) {
	if t, ok := superloop.ParseTaken(e.At, e.Verb, e.Subject, e.Detail); ok {
		f.taken = append(f.taken, t)
		// A step taken is no longer failing.
		delete(f.failures, t.Key)
		if t.Kind == superloop.KindFile && strings.HasPrefix(t.Key, loopStuckKey) {
			// The episode's start ends the key; filed is by step and error.
			i := strings.LastIndexByte(t.Key, ' ')
			f.stuckFiled[t.Key[:max(i, 0)]] = e.At
		}
		return
	}
	key, rest, ok := Keyed(e.Detail)
	switch {
	case !ok:
	case e.Verb == LoopOwnerlessVerb:
		if since, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(rest, OwnerlessSince)); err == nil {
			f.ownerless[key] = since
		} else {
			delete(f.ownerless, key)
		}
	case e.Verb == LoopClaimVerb:
		f.claims[key] = LoopClaim{At: e.At, Target: rest}
	case e.Verb == LoopClaimEndVerb:
		delete(f.claims, key)
		delete(f.failures, key)
		f.taken = slices.DeleteFunc(f.taken, func(t superloop.Taken) bool { return t.Key == key })
	case e.Verb == LoopStageVerb:
		f.stages[key] = true
		f.stageDetail[key] = rest
	case e.Verb == LoopUnstageVerb:
		delete(f.stages, key)
		delete(f.stageDetail, key)
	case e.Verb == LoopFailureVerb:
		var lf LoopFailure
		if json.Unmarshal([]byte(rest), &lf) == nil {
			// Failures an older loop logged carry neither time.
			lf.At = cmp.Or(lf.At, e.At)
			lf.Since = cmp.Or(lf.Since, e.At)
			kind, _, _ := strings.Cut(key, " ")
			lf.Kind = cmp.Or(lf.Kind, kind)
			lf.Ref = cmp.Or(lf.Ref, e.Subject)
			f.failures[key] = lf
		}
	}
}
