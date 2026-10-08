package cmd

import (
	"context"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/upgrade"
)

// readUpgrades is the running upgrades of alerts.installations: an
// installation's shared reading (upgrade.ReadingsFile) while it is younger
// than upgrades.every, a fresh one of the others.
func (a *app) readUpgrades(ctx context.Context, st *state.State, now time.Time) []upgrade.Status {
	return a.upgradeStatuses(ctx, st, now, upgrade.Readings{}, func(string) time.Duration { return a.cfg.Upgrades.Every.Duration })
}

// upgradeStatuses is the upgrade status of every installation of
// alerts.installations, in their order. rs, merged with the shared readings,
// answers for an installation whose reading began less than fresh(name)
// ago; the others are read, each within alerts.timeout, in parallel, and
// kept in rs and the shared readings for the next watch, snapshot or ui.
func (a *app) upgradeStatuses(ctx context.Context, st *state.State, now time.Time, rs upgrade.Readings, fresh func(installation string) time.Duration) []upgrade.Status {
	al := a.cfg.Alerts
	var shared upgrade.Readings
	if found, err := a.store.ReadFile(upgrade.ReadingsFile, &shared); found && err == nil {
		rs.Merge(shared)
	}
	var due []config.Installation
	for _, in := range al.Installations {
		if !rs.Fresh(in.Name, now, fresh(in.Name)) {
			due = append(due, in)
		}
	}
	if len(due) > 0 {
		var contexts []string
		if slices.ContainsFunc(due, func(in config.Installation) bool { return in.Context == "" }) {
			contexts = alerts.Reader{Kubectl: al.Kubectl}.Contexts(ctx)
		}
		targets := make([]upgrade.Target, 0, len(due))
		for _, in := range due {
			targets = append(targets, upgrade.Target{Name: in.Name, Context: alerts.ResolveContext(in.Name, in.Context, a.cfg.Kube.Context, contexts)})
		}
		r := upgrade.Reader{Kubectl: al.Kubectl, Timeout: al.Timeout.Duration}
		read := r.Read(ctx, targets, now, upgrade.HeldClusters(st, now))
		if ctx.Err() == nil {
			rs.Keep(read, now)
			_ = a.store.WriteFile(upgrade.ReadingsFile, rs)
		}
	}
	out := make([]upgrade.Status, 0, len(al.Installations))
	for _, in := range al.Installations {
		r, ok := rs[in.Name]
		if !ok {
			r.Status = upgrade.Status{Installation: in.Name, Err: "not read"}
		}
		out = append(out, r.Status)
	}
	return out
}

// upgradeFresh is how long the watch uses an installation's reading: up to
// upgrades.every, and one watch.interval while an upgrade runs on it or
// holds it, so its end and the hold's lift are said within one interval.
// Half an interval of slack lets a reading due at a tick be read at it.
func (w *watcher) upgradeFresh(st *state.State, now time.Time) func(string) time.Duration {
	interval := w.cfg.Watch.Interval.Duration
	return func(installation string) time.Duration {
		if _, held := upgrade.Held(st, installation, now); held || w.upgrades.Running(installation) {
			return interval / 2
		}
		return w.cfg.Upgrades.Every.Duration - interval/2
	}
}

// upgradeWatchFile is the state store's side file in which the
// supervisor's watch (a watch without --standby) says when its upgrade cycle
// last began.
const upgradeWatchFile = "upgrades-watch.json"

// upgradeBeat is upgradeWatchFile's content.
type upgradeBeat struct {
	At time.Time `json:"at"`
}

// upgradeWatchLive says whether a supervisor's watch began an upgrade cycle
// within slowReads+1 intervals of now: its cycles run every interval, every
// slowReads intervals while the machine is strained.
func (w *watcher) upgradeWatchLive(now time.Time) bool {
	var b upgradeBeat
	found, err := w.store.ReadFile(upgradeWatchFile, &b)
	return found && err == nil && now.Sub(b.At) < (slowReads+1)*w.cfg.Watch.Interval.Duration
}

// upgradeCycle makes the upgrade holds those of the running upgrades. The
// watch whose update sets a hold says UPGRADE, the one whose update lifts it
// UPGRADE ENDED, so a second or restarted watch says neither again. An
// unreadable installation is one line until it is readable again and keeps
// its holds as they are. The standby watch runs it only while no
// supervisor's watch does, and says so once.
func (w *watcher) upgradeCycle(ctx context.Context) {
	now := w.clk().Now()
	if w.standby {
		live := w.upgradeWatchLive(now)
		w.check("upgrades-standby", !live, "UPGRADES read by the standby watch: no supervisor's watch reads them, so this one sets and lifts the upgrade holds")
		if live {
			return
		}
	} else {
		_ = w.store.WriteFile(upgradeWatchFile, upgradeBeat{At: now})
	}
	st, err := w.store.Read()
	if err != nil {
		w.fail("upgrades", "UPGRADES unknown: the state does not load (%v)", err)
		return
	}
	statuses := w.upgradeStatuses(ctx, st, now, w.upgrades, w.upgradeFresh(st, now))
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
		w.fail("upgrades", "UPGRADES unknown: the state does not load (%v)", err)
		return
	}
	w.clear("upgrades")
	for _, s := range statuses {
		if s.Err != "" {
			w.fail("upgrades:"+s.Installation, "UPGRADES %s unreadable: %s", s.Installation, s.Err)
		} else {
			w.clear("upgrades:" + s.Installation)
		}
	}
	for _, h := range begun {
		w.emitNow("upgrade", "%s", upgrade.Line(h))
	}
	for _, h := range ended {
		w.emitNow("upgrade", "%s", upgrade.EndLine(h))
	}
	w.saveMark()
}
