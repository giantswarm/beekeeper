package cmd

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

const loopGraphQLFile = "loop-graphql.json"
const loopGraphQLLimit = config.DefaultLoopGraphQLLimit

// The loop-owned watch samples the budget at most every 30 seconds. Keep
// 300 points outside the work allowance for two identities' probes and boundaries.
const loopGraphQLProbeReserve = 300

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
		return now.Before(s.Reset) && s.Remaining < floor+LoopGraphQLReserve
	}
	for _, account := range s.Accounts {
		if now.Before(account.Reset) && account.Remaining < floor+LoopGraphQLReserve {
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

// loopGraphQLCap is supervisor.graphQLLimit, the allowance of the board's
// reads in a window.
func (a *app) loopGraphQLCap() int {
	if a == nil || a.cfg == nil || a.cfg.Supervisor.GraphQLLimit <= 0 {
		return loopGraphQLLimit
	}
	return a.cfg.Supervisor.GraphQLLimit
}

// overAllowance reports whether the spend leaves the board no room under
// limit.
func (s loopGraphQLSpend) overAllowance(limit int) bool {
	return s.Spent+LoopGraphQLReserve > limit-loopGraphQLProbeReserve
}

// heldLine says what the loop holds on s, "" when nothing: what whoever
// waits on the loop reads in budget and supervisor status. It reads the
// saved budget without the state lock, so a busy lock never stalls budget
// --gate behind a line it only prints (#3411).
func (a *app) heldLine(s loopGraphQLSpend) string {
	now := a.timeNow()
	st, err := a.store.Peek()
	if err != nil {
		return ""
	}
	var rest loopRESTSpend
	_, restErr := a.store.ReadFile(loopRESTFile, &rest)
	switch {
	case s.floorHeld(now, a.cfg.GitHub.Floor):
		return fmt.Sprintf("loop GitHub held: GraphQL under github.floor %d, no loop read until it recovers", a.cfg.GitHub.Floor)
	case restErr == nil && now.Before(rest.Reset) && rest.held(a.cfg.GitHub.Floor):
		return fmt.Sprintf("loop background scans held: REST %d of %d spent until %s; pull reads, reviews and merges go on", rest.Spent, rest.allowance(a.cfg.GitHub.Floor), clock(a.now, rest.Reset))
	case s.boardHeld(st.Budget, LoopBoard, a.cfg.GitHub.Floor, now, a.loopGraphQLCap()):
		return fmt.Sprintf("loop GitHub held: board reads wait until %s (%d of %d spent, supervisor.graphQLLimit); pull reads, reviews and merges go on", clock(a.now, s.Reset), s.Spent, a.loopGraphQLCap())
	}
	return ""
}

func (s loopGraphQLSpend) boardHeld(st *state.Budget, step string, floor int, now time.Time, limit int) bool {
	if step != LoopBoard || !now.Before(s.Reset) || !s.overAllowance(limit) {
		return false
	}
	if st != nil && st.GraphQL != nil {
		g := st.GraphQL
		if st.Remaining > floor && g.Remaining >= floor+LoopGraphQLReserve &&
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
		if step == "inventory" || step == LoopSweepStep {
			return a.loopScanREST(ctx, step, run)
		}
		return run()
	}
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	if st.Budget == nil || st.Budget.GraphQL == nil || BoardBudgetLow(st.Budget, a.cfg.GitHub.Floor, a.timeNow()) {
		return fmt.Errorf("loop GitHub budget unknown or low: %w", ErrLoopLater)
	}
	lock := flock.New(filepath.Join(a.store.Dir(), "loop-graphql.lock"))
	held, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return err
	}
	if !held {
		return ErrLoopLater
	}
	defer func() { _ = lock.Unlock() }()
	var spend loopGraphQLSpend
	if _, err := a.store.ReadFile(loopGraphQLFile, &spend); err != nil {
		return err
	}
	limit := a.loopGraphQLCap()
	if spend.boardHeld(st.Budget, step, a.cfg.GitHub.Floor, a.timeNow(), limit) {
		return fmt.Errorf("loop GitHub allowance held until %s: %w", spend.Reset, ErrLoopLater)
	}
	scan := step == "inventory" || step == LoopSweepStep
	if scan {
		if err := a.loopRESTHeld(a.timeNow()); err != nil {
			return err
		}
		ctx = context.WithValue(ctx, freshLoopRESTKey{}, true)
	}
	b, err := readLoopGraphQLBudget(a, ctx)
	if err != nil || b.GraphQL == nil || b.GraphQL.Err != "" || b.GraphQL.Blocks(a.timeNow()) {
		return fmt.Errorf("loop GitHub budget unavailable: %w", ErrLoopLater)
	}
	g := b.GraphQL
	reset := g.Reset
	if reset.IsZero() || !a.timeNow().Before(reset) {
		reset = a.timeNow().Add(time.Hour)
	}
	liveHealthy := b.Remaining > a.cfg.GitHub.Floor && g.Remaining >= a.cfg.GitHub.Floor+LoopGraphQLReserve
	wasHeld := spend.overAllowance(limit) || spend.floorHeld(a.timeNow(), a.cfg.GitHub.Floor)
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
	if !liveHealthy || (step == LoopBoard && spend.overAllowance(limit)) {
		spend.HeldAt = a.timeNow()
		if err := a.store.WriteFile(loopGraphQLFile, spend); err != nil {
			return err
		}
		return fmt.Errorf("loop GitHub held: %d points spent, %d remaining (floor %d): %w", spend.Spent, g.Remaining, a.cfg.GitHub.Floor, ErrLoopLater)
	}
	var restSpend loopRESTSpend
	if scan {
		restSpend, err = a.reserveLoopREST(b, step)
		if err != nil {
			return err
		}
	}
	spend.HeldAt = time.Time{}
	spend.Spent += LoopGraphQLReserve
	spend.Steps[step] += LoopGraphQLReserve
	if err := a.store.WriteFile(loopGraphQLFile, spend); err != nil {
		return err
	}
	runErr := run()
	after, err := readLoopGraphQLBudget(a, ctx)
	if err != nil || after.GraphQL == nil || after.GraphQL.Err != "" {
		return fmt.Errorf("loop GitHub spend unknown; reservation retained: %w", ErrLoopLater)
	}
	if scan {
		if err := a.settleLoopREST(b, after, restSpend, step); err != nil {
			return err
		}
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
		spend.Spent += cost - LoopGraphQLReserve
		spend.Steps[step] += cost - LoopGraphQLReserve
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

// refreshLoopBudget takes and saves a fresh budget reading, the rate limit
// reads alone: it spends no allowance and is not admitted under the floor.
var refreshLoopBudget = func(a *app, ctx context.Context) error {
	_, err := readLoopGraphQLBudget(a, ctx)
	return err
}

// Shell probes and local conditions stay outside the GitHub lock. Only
// checks the timer parser identifies as GitHub reads spend this allowance.
func (a *app) loopTimerChecks(ctx context.Context, timers []state.Timer, now time.Time, held bool) map[int]checkResult {
	var local, remote []state.Timer
	for _, t := range timers {
		if !timerDispatchCheckDue(t, now) {
			continue
		}
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
