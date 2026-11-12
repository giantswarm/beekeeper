package cmd

import (
	"context"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const loopPullStep = "pull"

const loopSweepStep = "sweep"

// Keep room for an admitted query and for concurrent callers. The large
// board regression checks a whole walk below this per-request reserve.
const loopGraphQLReserve = 500

type loopPullObservation struct {
	pull    superloop.Pull
	at      time.Time
	changed time.Time
}

func loopPullChanged(in superloop.Input, ref string) time.Time {
	var changed time.Time
	mark := func(at time.Time) {
		if at.After(changed) {
			changed = at
		}
	}
	var worker string
	for _, w := range in.Workers {
		if w.ParkedOn == ref {
			mark(w.ParkedAt)
			if worker == "" {
				worker = w.Name
			}
		}
	}
	for _, t := range in.Taken {
		if t.Ref == ref && t.Agent != "" {
			worker = t.Agent
		}
	}
	for _, a := range in.Answers {
		if a.Ref == ref {
			mark(a.At)
		}
	}
	reviewerPrefix := strings.TrimSuffix(superloop.ReviewerName(ref, 1), "1")
	for _, e := range in.Events {
		if worker != "" && e.Agent == worker {
			mark(e.At)
		}
		if rest, ok := strings.CutPrefix(e.Agent, reviewerPrefix); ok && rest != "" && strings.TrimLeft(rest, "0123456789") == "" {
			mark(e.At)
		}
	}
	return changed
}

func (l *supervisorLoop) observePull(ctx context.Context, ref string, changed time.Time) (superloop.Pull, error) {
	return l.observePullStep(ctx, ref, changed, loopPullStep)
}

// Observation lookup performs no IO against GitHub. Keep the original read
// time when copying a watcher's observation into this pass's action cache.
func (l *supervisorLoop) observePullStep(ctx context.Context, ref string, changed time.Time, step string) (superloop.Pull, error) {
	seen, err := readObservedLoopPull(l, ctx, ref, changed, step)
	if err != nil {
		return superloop.Pull{}, err
	}
	l.mu.Lock()
	if l.pulls == nil {
		l.pulls = map[string]loopPullObservation{}
	}
	l.pulls[ref] = seen
	l.mu.Unlock()
	return seen.pull, nil
}

// A reset is not evidence of recovery: wait for a reading from the new
// window, the watch's or the held pass's own (refreshLoopBudget). Unknown
// and stale observations fail closed.
func loopBudgetUnknownOrLow(b *state.Budget, floor int, now time.Time) bool {
	if b == nil || b.GraphQL == nil {
		return true
	}
	g := b.GraphQL
	return boardBudgetLow(b, floor, now) || g.Remaining <= floor || loopReadingStale(b, now)
}

// loopReadingStale: the saved GraphQL reading is missing, older than a
// minute or of a window that has reset; no reading the loop can hold on.
func loopReadingStale(b *state.Budget, now time.Time) bool {
	if b == nil || b.GraphQL == nil {
		return true
	}
	g := b.GraphQL
	return now.Sub(g.At) > time.Minute || (!g.Reset.IsZero() && !now.Before(g.Reset))
}
