package loop

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const LoopPullStep = "pull"

const LoopSweepStep = "sweep"

// LoopPassStep is the pass's own admission: it holds or admits the pass on
// the saved budget reading and takes no reading of its own (#3769).
const LoopPassStep = "pass"

// LoopRevisionStep is the conditional REST read of a repository's newest
// pull request: it spends no GraphQL, and unchanged no REST either, so it
// is admitted without the GraphQL readings that count a step's cost
// (#3769).
const LoopRevisionStep = "revision"

// Keep room for an admitted query and for concurrent callers. The large
// board regression checks a whole walk below this per-request reserve.
const LoopGraphQLReserve = 500

type LoopPullObservation struct {
	Pull    superloop.Pull
	At      time.Time
	Changed time.Time
}

func LoopPullChanged(in superloop.Input, ref string) time.Time {
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

func (l *Loop) ObservePull(ctx context.Context, ref string, changed time.Time) (superloop.Pull, error) {
	return l.ObservePullStep(ctx, ref, changed, LoopPullStep)
}

// Observation lookup performs no IO against GitHub. Keep the original read
// time when copying a watcher's observation into this pass's action cache.
func (l *Loop) ObservePullStep(ctx context.Context, ref string, changed time.Time, step string) (superloop.Pull, error) {
	seen, err := ReadObservedLoopPull(l, ctx, ref, changed, step)
	if err != nil {
		return superloop.Pull{}, err
	}
	l.Mu.Lock()
	if l.Pulls == nil {
		l.Pulls = map[string]LoopPullObservation{}
	}
	l.Pulls[ref] = seen
	l.Mu.Unlock()
	return seen.Pull, nil
}

// A reset is not evidence of recovery: wait for a reading from the new
// window, the watch's or the held pass's own (refreshLoopBudget). Unknown
// and stale observations fail closed.
func LoopBudgetUnknownOrLow(b *state.Budget, floor int, now time.Time) bool {
	if b == nil || b.GraphQL == nil {
		return true
	}
	g := b.GraphQL
	return BoardBudgetLow(b, floor, now) || g.Remaining <= floor || loopReadingStale(b, now)
}

// LoopBudgetHold says why a GitHub budget reading holds the loop's GitHub
// reads, "" when it admits them: LoopBudgetUnknownOrLow, or less GraphQL
// budget than reserve above github.floor for a read that keeps one. until
// is when the hold ends on its own, the reset of a low or refused window;
// fresh says a fresh reading may end it now: the reading is missing,
// failed, stale or of a window that has reset (#3841).
func LoopBudgetHold(b *state.Budget, floor, reserve int, now time.Time) (cause string, until time.Time, fresh bool) {
	if b == nil || b.GraphQL == nil {
		return "GitHub budget unknown: no reading", time.Time{}, true
	}
	g := b.GraphQL
	if !LoopBudgetUnknownOrLow(b, floor, now) && g.Remaining >= floor+reserve {
		return "", time.Time{}, false
	}
	open := now.Before(g.Reset) || g.Reset.IsZero() && now.Sub(g.At) < time.Hour
	switch {
	case g.Err != "":
		return "GitHub GraphQL budget unknown: " + g.Err, time.Time{}, true
	case g.Blocks(now) && g.Reset.IsZero():
		return "GitHub refuses GraphQL", time.Time{}, false
	case g.Blocks(now):
		return "GitHub refuses GraphQL until " + g.Reset.Local().Format("15:04"), g.Reset, false
	case Cmd.LowBudget(b, floor, now):
		return fmt.Sprintf("GitHub REST budget %d of %d under github.floor %d until %s",
			b.Remaining, b.Limit, floor, b.Reset.Local().Format("15:04")), b.Reset, false
	case open && g.Remaining <= floor:
		return fmt.Sprintf("GitHub GraphQL budget %d of %d under github.floor %d until %s",
			g.Remaining, g.Limit, floor, g.Reset.Local().Format("15:04")), g.Reset, false
	case open && g.Remaining < floor+reserve:
		return fmt.Sprintf("GitHub GraphQL budget %d of %d under github.floor %d and the %d kept above it until %s",
			g.Remaining, g.Limit, floor, reserve, g.Reset.Local().Format("15:04")), g.Reset, false
	case !g.Reset.IsZero() && !now.Before(g.Reset):
		return "GitHub GraphQL budget reading of a window that reset at " + g.Reset.Local().Format("15:04:05"), time.Time{}, true
	}
	return "GitHub GraphQL budget reading of " + g.At.Local().Format("15:04:05") + " is stale", time.Time{}, true
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
