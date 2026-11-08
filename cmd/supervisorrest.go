package cmd

import (
	"fmt"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
)

// Background observations have their own allowance. Merges, starts and
// workers retain the shared floor and the rest of the hourly core budget.
const loopRESTFile = "loop-rest.json"
const loopRESTLimit = 1000
const loopRESTReserve = 50

type loopRESTSpend struct {
	Reset     time.Time      `json:"reset"`
	Spent     int            `json:"spent"`
	Remaining int            `json:"remaining"`
	Steps     map[string]int `json:"steps,omitempty"`
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
	if spend.Spent+loopRESTReserve > loopRESTLimit || b.Remaining < a.cfg.GitHub.Floor+loopRESTReserve {
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
