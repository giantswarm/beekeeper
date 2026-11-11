package cmd

import (
	"context"
	"time"
)

// Coalesce atomic replacement and outbox events, and bound retries when many
// independent writers are active. Cancellation never waits for the debounce.
func pauseChange(ctx context.Context) bool {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
