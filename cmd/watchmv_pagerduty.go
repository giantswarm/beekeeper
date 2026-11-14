package cmd

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/alerts"
)

// watchPagerDuty reads the open incidents every alerts.pagerduty.every
// while this watch owns their baseline, at the same pace on a strained
// machine: a reading is two muster calls, and a page waits for none.
func (w *Watcher) watchPagerDuty(ctx context.Context) {
	store := alerts.NewPagerDutyStore(w.Host.cfg.StateDir)
	defer func() { _ = store.Release() }()
	other := 0
	w.Loop(ctx, w.Host.cfg.Alerts.PagerDuty.Every.Duration, false, func(ctx context.Context) {
		owned, owner, err := store.Own()
		switch {
		case err != nil:
			w.Emit("pagerduty", "PAGERDUTY baseline unusable: %v", err)
		case !owned:
			w.Clear("pagerduty")
			if owner.PID != other {
				other = owner.PID
				w.EmitNow("pagerduty", "PAGERDUTY read by the watch with pid %d; this one takes over when it ends", other)
			}
		default:
			w.Clear("pagerduty")
			if other != 0 {
				other = 0
				w.EmitNow("pagerduty", "PAGERDUTY taken over by this watch")
			}
			for _, l := range w.Host.pagerDutyCycle(ctx, store, w.musterRun) {
				w.EmitNow("pagerduty", "%s", l)
			}
		}
	})
}
