package cmd

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/alerts"
)

// watchPagerDuty reads the open incidents every alerts.pagerduty.every
// while this watch owns their baseline, at the same pace on a strained
// machine: a reading is two muster calls, and a page waits for none.
func (w *watcher) watchPagerDuty(ctx context.Context) {
	store := alerts.NewPagerDutyStore(w.a.cfg.StateDir)
	defer func() { _ = store.Release() }()
	other := 0
	w.loop(ctx, w.a.cfg.Alerts.PagerDuty.Every.Duration, false, func(ctx context.Context) {
		owned, owner, err := store.Own()
		switch {
		case err != nil:
			w.emit("pagerduty", "PAGERDUTY baseline unusable: %v", err)
		case !owned:
			w.clear("pagerduty")
			if owner.PID != other {
				other = owner.PID
				w.emitNow("pagerduty", "PAGERDUTY read by the watch with pid %d; this one takes over when it ends", other)
			}
		default:
			w.clear("pagerduty")
			if other != 0 {
				other = 0
				w.emitNow("pagerduty", "PAGERDUTY taken over by this watch")
			}
			for _, l := range w.a.pagerDutyCycle(ctx, store, w.musterRun) {
				w.emitNow("pagerduty", "%s", l)
			}
		}
	})
}
