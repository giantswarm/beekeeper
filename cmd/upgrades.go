package cmd

import (
	"context"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/upgrade"
)

// readUpgrades reads the running upgrades of alerts.installations, each
// installation within alerts.timeout, in parallel.
func (a *app) readUpgrades(ctx context.Context, st *state.State, now time.Time) []upgrade.Status {
	al := a.cfg.Alerts
	contexts := alerts.Reader{Kubectl: al.Kubectl}.Contexts(ctx)
	targets := make([]upgrade.Target, 0, len(al.Installations))
	for _, in := range al.Installations {
		targets = append(targets, upgrade.Target{Name: in.Name, Context: alerts.ResolveContext(in.Name, in.Context, contexts)})
	}
	r := upgrade.Reader{Kubectl: al.Kubectl, Timeout: al.Timeout.Duration}
	return r.Read(ctx, targets, now, upgrade.HeldClusters(st, now))
}

// watchUpgrades reads the upgrades every watch.interval.
func (w *watcher) watchUpgrades(ctx context.Context) {
	for {
		start := time.Now()
		w.upgradeCycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(max(time.Until(start.Add(w.cfg.Watch.Interval.Duration)), 0)):
		}
	}
}

// upgradeCycle makes the upgrade holds those of the running upgrades. The
// watch whose update sets a hold says UPGRADE, the one whose update lifts it
// UPGRADE ENDED, so a second or restarted watch says neither again. An
// unreadable installation is one line until it is readable again and keeps
// its holds as they are.
func (w *watcher) upgradeCycle(ctx context.Context) {
	now := time.Now()
	st, err := w.store.Read()
	if err != nil {
		w.emit("upgrades", "UPGRADES unknown: the state does not load (%v)", err)
		return
	}
	statuses := w.readUpgrades(ctx, st, now)
	if ctx.Err() != nil {
		return
	}
	var begun, ended []state.Hold
	err = w.store.Update(func(st *state.State) ([]state.Event, error) {
		begun, ended = nil, nil
		var ev []state.Event
		for _, s := range statuses {
			if s.Err != "" {
				continue
			}
			b, e := upgrade.Reconcile(st, s.Installation, s.Upgrades, now, watchParty)
			for _, h := range b {
				ev = append(ev, event(watchParty, "hold.set", "%s until the upgrade ends: %s", h.Target, h.Reason))
			}
			for _, h := range e {
				ev = append(ev, event(watchParty, "hold.lift", "%s: the upgrade ended", h.Target))
			}
			begun, ended = append(begun, b...), append(ended, e...)
		}
		return ev, nil
	})
	if err != nil {
		w.emit("upgrades", "UPGRADES unknown: the state does not load (%v)", err)
		return
	}
	w.clear("upgrades")
	for _, s := range statuses {
		w.check("upgrades:"+s.Installation, s.Err != "", "UPGRADES %s unreadable: %s", s.Installation, s.Err)
	}
	for _, h := range begun {
		w.emitNow("upgrade", "%s", upgrade.Line(h))
	}
	for _, h := range ended {
		w.emitNow("upgrade", "%s", upgrade.EndLine(h))
	}
	w.saveMark()
}
