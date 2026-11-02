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

// The loop-owned watch samples the budget at most every 30 seconds. Keep
// 300 points outside the work allowance for two identities' probes and boundaries.
const loopGraphQLProbeReserve = 300

// Keep room for an admitted query and for concurrent callers. The large
// board regression checks a whole walk below this per-request reserve.
const loopGraphQLReserve = 500

type loopGraphQLSpend struct {
	Reset     time.Time                     `json:"reset"`
	Remaining int                           `json:"remaining"`
	Spent     int                           `json:"spent"`
	Steps     map[string]int                `json:"steps"`
	Accounts  map[string]loopGraphQLAccount `json:"accounts,omitempty"`
}

type loopGraphQLAccount struct {
	Reset     time.Time `json:"reset"`
	Remaining int       `json:"remaining"`
}

func graphQLAccounts(b github.Budget) map[string]loopGraphQLAccount {
	out := map[string]loopGraphQLAccount{}
	add := func(id string, g *github.GraphQL) {
		if g != nil {
			if g.Login != "" {
				id += "/" + g.Login
			}
			out[id] = loopGraphQLAccount{Reset: g.Reset, Remaining: g.Remaining}
		}
	}
	if len(b.Identities) == 0 {
		if b.GraphQL != nil {
			add(b.GraphQL.Identity, b.GraphQL)
		}
	} else {
		for id, profile := range b.Identities {
			if len(profile.Windows) == 0 {
				add(id, profile.GraphQL)
			} else {
				for key, window := range profile.Windows {
					add(id+"@"+key, &window)
				}
			}
		}
	}
	return out
}

func (s loopGraphQLSpend) floorHeld(now time.Time, floor int) bool {
	if len(s.Accounts) == 0 {
		return now.Before(s.Reset) && s.Remaining < floor+loopGraphQLReserve
	}
	for _, account := range s.Accounts {
		if now.Before(account.Reset) && account.Remaining < floor+loopGraphQLReserve {
			return true
		}
	}
	return false
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
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	if st.Budget == nil || st.Budget.GraphQL == nil || boardBudgetLow(st.Budget, a.cfg.GitHub.Floor, a.now) {
		return fmt.Errorf("loop GitHub budget unknown or low: %w", errLoopLater)
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
	if a.now.Before(spend.Reset) && spend.Spent+loopGraphQLReserve > loopGraphQLLimit-loopGraphQLProbeReserve || spend.floorHeld(a.now, a.cfg.GitHub.Floor) {
		return fmt.Errorf("loop GitHub allowance held until %s: %w", spend.Reset, errLoopLater)
	}
	b, err := readLoopGraphQLBudget(a, ctx)
	if err != nil || b.GraphQL == nil || b.GraphQL.Err != "" || b.GraphQL.Blocks(a.now) {
		return fmt.Errorf("loop GitHub budget unavailable: %w", errLoopLater)
	}
	g := b.GraphQL
	// This allowance has its own hour. Selecting another identity or a
	// GitHub window resetting must never replenish the loop's allowance.
	if !a.now.Before(spend.Reset) || spend.Steps == nil {
		spend = loopGraphQLSpend{Reset: a.now.Add(time.Hour), Remaining: g.Remaining, Steps: map[string]int{}}
	}
	accounts := graphQLAccounts(b)
	shared := 0
	for id, current := range accounts {
		if previous, ok := spend.Accounts[id]; ok && previous.Reset.Equal(current.Reset) {
			shared += max(0, previous.Remaining-current.Remaining)
		}
	}
	spend.Spent += shared
	spend.Steps["shared"] += shared
	spend.Remaining = g.Remaining
	spend.Accounts = accounts
	if b.Remaining <= a.cfg.GitHub.Floor || g.Remaining < a.cfg.GitHub.Floor+loopGraphQLReserve || spend.Spent+loopGraphQLReserve > loopGraphQLLimit-loopGraphQLProbeReserve {
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
	// Match counters by identity and window, never by the aggregate minimum.
	afterAccounts := graphQLAccounts(after)
	known := len(afterAccounts) == len(accounts)
	cost := 0
	for id, previous := range accounts {
		current, ok := afterAccounts[id]
		if !ok || !current.Reset.Equal(previous.Reset) {
			known = false
			continue
		}
		cost += max(0, previous.Remaining-current.Remaining)
	}
	if known {
		spend.Spent += cost - loopGraphQLReserve
		spend.Steps[step] += cost - loopGraphQLReserve
		spend.Remaining = after.GraphQL.Remaining
		spend.Accounts = afterAccounts
		if err := a.store.WriteFile(loopGraphQLFile, spend); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("loop GitHub identity or window changed; reservation retained: %w", errLoopLater)
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

// Shell probes and local conditions stay outside the GitHub lock. Only
// checks the timer parser identifies as GitHub reads spend this allowance.
func (a *app) loopTimerChecks(ctx context.Context, timers []state.Timer, now time.Time, held bool) map[int]checkResult {
	var local, remote []state.Timer
	for _, t := range timers {
		if checkOf(t).github {
			remote = append(remote, t)
		} else {
			local = append(local, t)
		}
	}
	found := checkTimers(ctx, local, now, held)
	if held || len(remote) == 0 {
		return found
	}
	var checked map[int]checkResult
	if err := a.loopGitHub(ctx, "timers", func() error {
		checked = checkTimers(ctx, remote, now, false)
		return nil
	}); err == nil {
		for id, result := range checked {
			found[id] = result
		}
	}
	return found
}
