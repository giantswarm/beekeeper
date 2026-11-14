package watch

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/state"
)

// overrides settles the overrides whose end arrived, a GitHub check held
// while githubHeld, and says what it did.
func (w *Watcher) overrides(ctx context.Context, githubHeld bool) {
	lines, err := w.Host.SettleOverrides(ctx, WatchParty, func(ts []state.Timer) map[int]CheckResult {
		if w.LoopOwned {
			return w.Host.LoopTimerChecks(ctx, ts, w.Host.Now(), githubHeld)
		}
		return Cmd.CheckTimers(ctx, ts, w.Host.Now(), githubHeld)
	})
	for _, l := range lines {
		w.EmitNow("pending", "%s", l)
	}
	if err != nil {
		w.EmitNow("pending", "CONFIG OVERRIDES FAILED: %v", err)
	}
}
