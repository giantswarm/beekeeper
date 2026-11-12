package cmd

import (
	"context"
	"time"

	clk "github.com/giantswarm/beekeeper/internal/clock"
)

// Coalesce atomic replacement and outbox events, and bound retries when many
// independent writers are active. Cancellation never waits for the debounce.
func pauseChange(ctx context.Context) bool {
	return pauseChangeFor(ctx, 100*time.Millisecond)
}

// passChangeGap is the shortest time between the end of a supervisor pass
// and a pass a state change wakes. Every session, runner and command writes
// the state; without a floor a busy desk keeps the loop passing back to back,
// each pass re-reading the state and the event log.
var passChangeGap = 5 * time.Second

// pauseChangeFor is pauseChange for at least d, never shorter than the
// debounce.
func pauseChangeFor(ctx context.Context, d time.Duration) bool {
	timer := clk.NewTimer(max(d, 100*time.Millisecond))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
