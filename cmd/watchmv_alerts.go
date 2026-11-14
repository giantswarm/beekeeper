package cmd

import (
	"context"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
)

// watchDiscovery discovers the installations that carry the team's
// HelmReleases every alerts.discover.every, beginning with a restart only
// once the kept discovery is that old, and says the ones that joined or
// left; a failed listing of Teleport's clusters is a lasting condition.
func (w *watcher) watchDiscovery(ctx context.Context) {
	every := w.a.cfg.Alerts.Discover.Every.Duration
	w.loop(ctx, every, true, func(ctx context.Context) {
		if d, err := alerts.LoadDiscovery(w.a.cfg.StateDir); err == nil && time.Since(d.At) < every-time.Minute {
			return
		}
		changed, _, err := w.a.alertDiscovery(ctx)
		if err != nil {
			if ctx.Err() == nil {
				w.emit("discover", "ALERTS discovery failed: %v", err)
			}
			return
		}
		w.clear("discover")
		for _, l := range changed {
			w.emitNow("alerts", "%s", l)
		}
	})
}
