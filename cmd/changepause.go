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
