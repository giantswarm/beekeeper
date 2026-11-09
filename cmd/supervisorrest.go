package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/github"
)

// Background observations have their own allowance. Merges, starts and
// workers retain the shared floor and the rest of the hourly core budget.
const loopRESTFile = "loop-rest.json"
const loopRESTLimit = 1000
const loopRESTReserve = 50

// Emergency access disables the shared floor, not the background scan
// allowance. Keep that allowance independent of GraphQL admission so scans
// cannot consume the requests the emergency access is meant to preserve.
func (a *app) loopScanREST(ctx context.Context, step string, run func() error) error {
	lock := flock.New(filepath.Join(a.store.Dir(), "loop-graphql.lock"))
	held, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return err
	}
	if !held {
		return errLoopLater
	}
	defer func() { _ = lock.Unlock() }()
	if err := a.loopRESTHeld(a.timeNow()); err != nil {
		return err
	}
	ctx = context.WithValue(ctx, freshLoopRESTKey{}, true)
	before, err := readLoopGraphQLBudget(a, ctx)
	if err != nil {
		return fmt.Errorf("loop REST budget unavailable: %w", errLoopLater)
	}
	spend, err := a.reserveLoopREST(before, step)
	if err != nil {
		return err
	}
	runErr := run()
	after, err := readLoopGraphQLBudget(a, ctx)
	if err != nil {
		return fmt.Errorf("loop REST spend unknown; reservation retained: %w", errLoopLater)
	}
	if err := a.settleLoopREST(before, after, spend, step); err != nil {
		return err
	}
	return runErr
}

type loopRESTSpend struct {
	Reset     time.Time      `json:"reset"`
	Spent     int            `json:"spent"`
	Remaining int            `json:"remaining"`
	Steps     map[string]int `json:"steps,omitempty"`
}

func (a *app) loopRESTHeld(now time.Time) error {
	var spend loopRESTSpend
	if _, err := a.store.ReadFile(loopRESTFile, &spend); err != nil {
		return err
	}
	if spend.Reset.IsZero() || !now.Before(spend.Reset) || spend.Spent+loopRESTReserve <= loopRESTLimit {
		return nil
	}
	if st, err := a.store.Read(); err == nil && st.Budget != nil && !st.Budget.Reset.IsZero() && !sameGraphQLWindow(spend.Reset, st.Budget.Reset) {
		return nil
	}
	return fmt.Errorf("loop REST held: %d of %d spent, %d remaining (floor %d), reset %s: %w", spend.Spent, loopRESTLimit, spend.Remaining, a.cfg.GitHub.Floor, spend.Reset.UTC().Format(time.RFC3339), errLoopLater)
}

// Called under loop-graphql.lock, so parallel loop actions share one ledger.
func (a *app) reserveLoopREST(b github.Budget, step string) (loopRESTSpend, error) {
	var spend loopRESTSpend
	if _, err := a.store.ReadFile(loopRESTFile, &spend); err != nil {
		return spend, err
	}
	now := a.timeNow()
	if b.Reset.IsZero() || !now.Before(b.Reset) {
		return spend, fmt.Errorf("loop REST budget window unknown: %w", errLoopLater)
	}
	if spend.Reset.IsZero() || !sameGraphQLWindow(spend.Reset, b.Reset) {
		spend = loopRESTSpend{Reset: b.Reset, Steps: map[string]int{}}
	}
	if spend.Steps == nil {
		spend.Steps = map[string]int{}
	}
	spend.Remaining = b.Remaining
	if spend.Spent+loopRESTReserve > loopRESTLimit || b.Remaining < max(0, a.cfg.GitHub.Floor)+loopRESTReserve {
		return spend, fmt.Errorf("loop REST held: %d of %d spent, %d remaining (floor %d), reset %s: %w", spend.Spent, loopRESTLimit, b.Remaining, a.cfg.GitHub.Floor, spend.Reset.UTC().Format(time.RFC3339), errLoopLater)
	}
	spend.Spent += loopRESTReserve
	spend.Steps[step] += loopRESTReserve
	return spend, a.store.WriteFile(loopRESTFile, spend)
}

func (a *app) settleLoopREST(before, after github.Budget, spend loopRESTSpend, step string) error {
	// Retain the reservation when the identity or window changed: comparing
	// unrelated readings would falsely refund work whose cost is unknown.
	if !sameGraphQLWindow(before.Reset, after.Reset) || before.Identity != after.Identity || before.Login != after.Login || after.Remaining > before.Remaining {
		return nil
	}
	cost := max(0, before.Remaining-after.Remaining)
	spend.Spent += cost - loopRESTReserve
	spend.Steps[step] += cost - loopRESTReserve
	spend.Remaining = after.Remaining
	return a.store.WriteFile(loopRESTFile, spend)
}
