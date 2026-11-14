package cmd

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/state"
)

// overrides settles the overrides whose end arrived, a GitHub check held
// while githubHeld, and says what it did.
func (w *watcher) overrides(ctx context.Context, githubHeld bool) {
	lines, err := w.a.settleOverrides(ctx, watchParty, func(ts []state.Timer) map[int]checkResult {
		if w.loopOwned {
			return w.a.loopTimerChecks(ctx, ts, w.a.now, githubHeld)
		}
		return checkTimers(ctx, ts, w.a.now, githubHeld)
	})
	for _, l := range lines {
		w.emitNow("pending", "%s", l)
	}
	if err != nil {
		w.emitNow("pending", "CONFIG OVERRIDES FAILED: %v", err)
	}
}
