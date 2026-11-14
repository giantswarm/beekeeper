package cmd

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/state"
)

// overrides settles the overrides whose end arrived, a GitHub check held
// while githubHeld, and says what it did.
func (w *Watcher) overrides(ctx context.Context, githubHeld bool) {
	lines, err := w.Host.settleOverrides(ctx, WatchParty, func(ts []state.Timer) map[int]checkResult {
		if w.LoopOwned {
			return w.Host.loopTimerChecks(ctx, ts, w.Host.now, githubHeld)
		}
		return checkTimers(ctx, ts, w.Host.now, githubHeld)
	})
	for _, l := range lines {
		w.EmitNow("pending", "%s", l)
	}
	if err != nil {
		w.EmitNow("pending", "CONFIG OVERRIDES FAILED: %v", err)
	}
}
