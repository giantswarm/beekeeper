package cmd

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const loopGraphQLFile = "loop-graphql.json"
const loopGraphQLLimit = 1500
const loopPullStep = "pull"

// The loop-owned watch samples the budget at most every 30 seconds. Keep
// 300 points outside the work allowance for two identities' probes and boundaries.
const loopGraphQLProbeReserve = 300

// Keep room for an admitted query and for concurrent callers. The large
// board regression checks a whole walk below this per-request reserve.
const loopGraphQLReserve = 500

type loopGraphQLSpend struct {
	Reset     time.Time                     `json:"reset"`
	HeldAt    time.Time                     `json:"heldAt,omitzero"`
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
		if g == nil {
			return
		}
		key := cmp.Or(g.Login, id, "default")
		if old, ok := out[key]; !ok || g.Remaining < old.Remaining {
			out[key] = loopGraphQLAccount{Reset: g.Reset, Remaining: g.Remaining}
		}
	}
	if len(b.Identities) == 0 {
		if b.GraphQL != nil {
			add(b.GraphQL.Identity, b.GraphQL)
		}
	} else {
		for id, profile := range b.Identities {
			add(id, profile.GraphQL)
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

func sameGraphQLWindow(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return a.Equal(b)
	}
	diff := a.Sub(b)
	if diff < 0 {
		diff = -diff
	}
	return diff <= 2*time.Minute
}

func (s loopGraphQLSpend) boardHeld(st *state.Budget, step string, floor int, now time.Time) bool {
	if step != loopBoard || !now.Before(s.Reset) || s.Spent+loopGraphQLReserve <= loopGraphQLLimit-loopGraphQLProbeReserve {
		return false
	}
	if st != nil && st.GraphQL != nil {
		g := st.GraphQL
		if st.Remaining > floor && g.Remaining >= floor+loopGraphQLReserve &&
			(s.floorHeld(now, floor) || g.Limit > 0 && g.Limit-g.Remaining < s.Spent || g.Remaining > s.Remaining || (!g.Reset.IsZero() && !sameGraphQLWindow(s.Reset, g.Reset))) {
			return false
		}
	}
	return true
}

var readLoopGraphQLBudget = func(a *app, ctx context.Context) (github.Budget, error) {
	copyApp, cfg := *a, *a.cfg
	cfg.Merge.BudgetFresh.Duration = -time.Second
	copyApp.cfg = &cfg
	return copyApp.probeBudget(ctx)
}

// loopGitHub admits one observation against the shared floor and a durable
// allowance for the active GraphQL window. A healthy live REST and GraphQL
// budget reading above the floor releases a held allowance rather than
// renewing a blind hourly hold.
func (a *app) loopGitHub(ctx context.Context, step string, run func() error) error {
	if a.cfg.GitHub.Floor <= 0 {
		return run()
	}
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	if st.Budget == nil || st.Budget.GraphQL == nil || boardBudgetLow(st.Budget, a.cfg.GitHub.Floor, a.timeNow()) {
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
	if spend.boardHeld(st.Budget, step, a.cfg.GitHub.Floor, a.timeNow()) {
		return fmt.Errorf("loop GitHub allowance held until %s: %w", spend.Reset, errLoopLater)
	}
	b, err := readLoopGraphQLBudget(a, ctx)
	if err != nil || b.GraphQL == nil || b.GraphQL.Err != "" || b.GraphQL.Blocks(a.timeNow()) {
		return fmt.Errorf("loop GitHub budget unavailable: %w", errLoopLater)
	}
	g := b.GraphQL
	reset := g.Reset
	if reset.IsZero() || !a.timeNow().Before(reset) {
		reset = a.timeNow().Add(time.Hour)
	}
	liveHealthy := b.Remaining > a.cfg.GitHub.Floor && g.Remaining >= a.cfg.GitHub.Floor+loopGraphQLReserve
	wasHeld := spend.Spent+loopGraphQLReserve > loopGraphQLLimit-loopGraphQLProbeReserve || spend.floorHeld(a.timeNow(), a.cfg.GitHub.Floor)
	releaseHold := liveHealthy && wasHeld && (spend.floorHeld(a.timeNow(), a.cfg.GitHub.Floor) ||
		(!g.Reset.IsZero() && !sameGraphQLWindow(spend.Reset, g.Reset)) ||
		g.Remaining > spend.Remaining ||
		(g.Limit > 0 && g.Limit-g.Remaining < spend.Spent))
	if !a.timeNow().Before(spend.Reset) || spend.Steps == nil || (!g.Reset.IsZero() && !sameGraphQLWindow(spend.Reset, g.Reset)) || releaseHold {
		spend = loopGraphQLSpend{Reset: reset, Remaining: g.Remaining, Steps: map[string]int{}}
	}
	accounts := graphQLAccounts(b)
	spend.Remaining = g.Remaining
	spend.Accounts = accounts
	if !liveHealthy || (step == loopBoard && spend.Spent+loopGraphQLReserve > loopGraphQLLimit-loopGraphQLProbeReserve) {
		spend.HeldAt = a.timeNow()
		if err := a.store.WriteFile(loopGraphQLFile, spend); err != nil {
			return err
		}
		return fmt.Errorf("loop GitHub held: %d points spent, %d remaining (floor %d): %w", spend.Spent, g.Remaining, a.cfg.GitHub.Floor, errLoopLater)
	}
	spend.HeldAt = time.Time{}
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
	afterAccounts := graphQLAccounts(after)
	cost := 0
	windowChanged := false
	for id, previous := range accounts {
		current, ok := afterAccounts[id]
		if !ok || !sameGraphQLWindow(current.Reset, previous.Reset) || current.Remaining > previous.Remaining {
			windowChanged = true
			continue
		}
		cost += max(0, previous.Remaining-current.Remaining)
	}
	if windowChanged {
		afterReset := after.GraphQL.Reset
		if afterReset.IsZero() || !a.timeNow().Before(afterReset) {
			afterReset = a.timeNow().Add(time.Hour)
		}
		used := 0
		if after.GraphQL.Limit > 0 {
			used = max(0, after.GraphQL.Limit-after.GraphQL.Remaining)
		}
		spend = loopGraphQLSpend{
			Reset:     afterReset,
			Remaining: after.GraphQL.Remaining,
			Spent:     used,
			Steps:     map[string]int{step: used},
			Accounts:  afterAccounts,
		}
	} else {
		spend.Spent += cost - loopGraphQLReserve
		spend.Steps[step] += cost - loopGraphQLReserve
		spend.Remaining = after.GraphQL.Remaining
		spend.Accounts = afterAccounts
	}
	if err := a.store.WriteFile(loopGraphQLFile, spend); err != nil {
		return err
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
	if seen, ok := l.pulls[ref]; ok && seen.changed.Equal(changed) && l.a.timeNow().Sub(seen.at) < loopPullEvery {
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
	at := l.a.timeNow()
	if l.a.loopNow != nil {
		if now := time.Now(); now.After(at) {
			at = now
		}
	}
	l.pulls[ref] = loopPullObservation{pull: p, at: at, changed: changed}
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
