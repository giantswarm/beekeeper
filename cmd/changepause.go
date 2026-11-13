package cmd

import (
	"context"
	"time"

	clk "github.com/giantswarm/beekeeper/internal/clock"
)

// feedChangeGap is the shortest time between the end of a guide watch poll and
// a poll a change of the state or the event log wakes. Every session, runner
// and command writes them; without a floor a busy desk keeps the feed reading
// the state back to back. The feed's own interval still polls on time.
var feedChangeGap = 2 * time.Second

// Coalesce atomic replacement and outbox events, and bound retries when many
// independent writers are active. Cancellation never waits for the debounce.
func pauseChange(ctx context.Context) bool {
	return pauseChangeFor(ctx, 100*time.Millisecond)
}

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
