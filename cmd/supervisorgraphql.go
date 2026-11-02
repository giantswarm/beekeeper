package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const loopGraphQLFile = "loop-graphql.json"
const loopGraphQLLimit = 1500

// Keep room for an admitted query and for concurrent callers. The large
// board regression checks a whole walk below this per-request reserve.
const loopGraphQLReserve = 500

type loopGraphQLSpend struct {
	Reset     time.Time      `json:"reset"`
	Remaining int            `json:"remaining"`
	Spent     int            `json:"spent"`
	Steps     map[string]int `json:"steps"`
}

var readLoopGraphQLBudget = func(a *app, ctx context.Context) (github.Budget, error) {
	copyApp, cfg := *a, *a.cfg
	cfg.Merge.BudgetFresh.Duration = -time.Second
	copyApp.cfg = &cfg
	return copyApp.probeBudget(ctx)
}

// loopGitHub admits one observation against the shared floor and a durable
// hourly allowance. Traffic between observations counts conservatively as
// shared spend: it can hold the loop early but cannot grant extra allowance.
// Reserving before the call makes an interrupted child fail closed too.
func (a *app) loopGitHub(ctx context.Context, step string, run func() error) error {
	if a.cfg.GitHub.Floor <= 0 {
		return run()
	}
	lock := flock.New(filepath.Join(a.store.Dir(), "loop-graphql.lock"))
	held, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return err
	}
	if !held {
		return errLoopLater
	}
	defer func() { _ = lock.Unlock() }()
	var spend loopGraphQLSpend
	if _, err := a.store.ReadFile(loopGraphQLFile, &spend); err != nil {
		return err
	}
	// Once this window is held, another budget probe cannot release it.
	// Repeated held attempts must cost zero GraphQL until its reset.
	if a.now.Before(spend.Reset) && (spend.Spent+loopGraphQLReserve > loopGraphQLLimit || spend.Remaining < a.cfg.GitHub.Floor+loopGraphQLReserve) {
		return fmt.Errorf("loop GitHub allowance held until %s: %w", spend.Reset, errLoopLater)
	}
	b, err := readLoopGraphQLBudget(a, ctx)
	if err != nil || b.GraphQL == nil || b.GraphQL.Err != "" || b.GraphQL.Blocks(a.now) {
		return fmt.Errorf("loop GitHub budget unavailable: %w", errLoopLater)
	}
	g := b.GraphQL
	if !spend.Reset.Equal(g.Reset) || spend.Steps == nil {
		spend = loopGraphQLSpend{Reset: g.Reset, Remaining: g.Remaining, Steps: map[string]int{}}
	}
	shared := max(0, spend.Remaining-g.Remaining)
	spend.Spent += shared
	spend.Steps["shared"] += shared
	spend.Remaining = g.Remaining
	if b.Remaining <= a.cfg.GitHub.Floor || g.Remaining < a.cfg.GitHub.Floor+loopGraphQLReserve || spend.Spent+loopGraphQLReserve > loopGraphQLLimit {
		if err := a.store.WriteFile(loopGraphQLFile, spend); err != nil {
			return err
		}
		return fmt.Errorf("loop GitHub held: %d points spent, %d remaining (floor %d): %w", spend.Spent, g.Remaining, a.cfg.GitHub.Floor, errLoopLater)
	}
	spend.Spent += loopGraphQLReserve
	spend.Steps[step] += loopGraphQLReserve
	if err := a.store.WriteFile(loopGraphQLFile, spend); err != nil {
		return err
	}
	runErr := run()
	after, err := readLoopGraphQLBudget(a, ctx)
	if err != nil || after.GraphQL == nil || after.GraphQL.Err != "" {
		return fmt.Errorf("loop GitHub spend unknown; reservation retained: %w", errLoopLater)
	}
	// A reset while the query ran cannot be charged to the next window.
	if after.GraphQL.Reset.Equal(g.Reset) {
		cost := max(0, g.Remaining-after.GraphQL.Remaining)
		spend.Spent += cost - loopGraphQLReserve
		spend.Steps[step] += cost - loopGraphQLReserve
		spend.Remaining = after.GraphQL.Remaining
		if err := a.store.WriteFile(loopGraphQLFile, spend); err != nil {
			return err
		}
	}
	return runErr
}

// A desk with 40 tracked PRs reads them at most twelve times an hour in
// steady state, rather than 120 times at the default loop interval.
const loopPullEvery = 5 * time.Minute

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
	for _, w := range in.Workers {
		if w.ParkedOn == ref {
			mark(w.ParkedAt)
		}
	}
	for _, a := range in.Answers {
		if a.Ref == ref {
			mark(a.At)
		}
	}
	for _, e := range in.Events {
		for round := 1; round <= superloop.MaxReviews; round++ {
			if e.Agent == superloop.ReviewerName(ref, round) {
				mark(e.At)
			}
		}
	}
	return changed
}

func (l *supervisorLoop) observePull(ctx context.Context, ref string, changed time.Time) (superloop.Pull, error) {
	if seen, ok := l.pulls[ref]; ok && seen.changed.Equal(changed) && l.a.now.Sub(seen.at) < loopPullEvery {
		return seen.pull, nil
	}
	var p superloop.Pull
	err := l.a.loopGitHub(ctx, "pull", func() (err error) {
		p, err = readLoopPull(ctx, ref)
		return err
	})
	if err != nil {
		return p, err
	}
	if l.pulls == nil {
		l.pulls = map[string]loopPullObservation{}
	}
	l.pulls[ref] = loopPullObservation{pull: p, at: l.a.now, changed: changed}
	return p, nil
}

// A reset is not evidence of recovery: wait for the watch to publish a
// reading from the new window. Unknown and stale observations fail closed.
func loopBudgetUnknownOrLow(b *state.Budget, floor int, now time.Time) bool {
	if b == nil || b.GraphQL == nil {
		return true
	}
	g := b.GraphQL
	return boardBudgetLow(b, floor, now) || g.Remaining <= floor ||
		now.Sub(g.At) > time.Minute || (!g.Reset.IsZero() && !now.Before(g.Reset))
}
