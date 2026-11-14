package cmd

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/upgrade"
)

// upgradeFresh is how long the watch uses an installation's reading: up to
// upgrades.every, and one watch.interval while an upgrade runs on it or
// holds it, so its end and the hold's lift are said within one interval.
// Half an interval of slack lets a reading due at a tick be read at it.
func (w *watcher) upgradeFresh(st *state.State, now time.Time) func(string) time.Duration {
	interval := w.a.cfg.Watch.Interval.Duration
	return func(installation string) time.Duration {
		if _, held := upgrade.Held(st, installation, now); held || w.upgrades.Running(installation) {
			return interval / 2
		}
		return w.a.cfg.Upgrades.Every.Duration - interval/2
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
	found, err := w.a.store.ReadFile(upgradeWatchFile, &b)
	return found && err == nil && now.Sub(b.At) < (slowReads+1)*w.a.cfg.Watch.Interval.Duration
}

// upgradeCycle makes the upgrade holds those of the running upgrades. The
// watch whose update sets a hold says UPGRADE, the one whose update lifts it
// UPGRADE ENDED, so a second or restarted watch says neither again. An
// unreadable installation is one line until it is readable again and keeps
// its holds as they are; one without a kube context keeps them too and is
// named once per watch (sayNoContext). The standby watch runs it only while no
// supervisor's watch does, and says so once.
func (w *watcher) upgradeCycle(ctx context.Context) {
	w.upgradeCycleRead(ctx, w.a.upgradeStatuses)
}

func (w *watcher) upgradeCycleRead(ctx context.Context, read func(context.Context, *state.State, time.Time, upgrade.Readings, func(string) time.Duration) []upgrade.Status) {
	now := w.clk().Now()
	if w.standby {
		live := w.upgradeWatchLive(now)
		w.check("upgrades-standby", !live, "UPGRADES read by the standby watch: no supervisor's watch reads them, so this one sets and lifts the upgrade holds")
		if live {
			return
		}
	} else {
		_ = w.a.store.WriteFile(upgradeWatchFile, upgradeBeat{At: now})
	}
	// Serialize observation and reconciliation across watches without holding
	// the desk state lock during Kubernetes reads. Those reads can take minutes;
	// timers, stop requests and other desk writers must remain able to settle.
	var statuses []upgrade.Status
	if dir := w.a.store.Dir(); dir != "" {
		cycle := flock.New(filepath.Join(dir, "upgrades-cycle.lock"))
		ok, err := cycle.TryLockContext(ctx, 10*time.Millisecond)
		if err != nil || !ok {
			if ctx.Err() == nil {
				w.fail("upgrades", "UPGRADES unknown: cycle lock unavailable (%v)", err)
			}
			return
		}
		defer func() { _ = cycle.Unlock() }()
		observed, err := w.a.store.Read()
		if err != nil {
			w.fail("upgrades", "UPGRADES unknown: the state does not load (%v)", err)
			return
		}
		now = w.clk().Now()
		statuses = read(ctx, observed, now, w.upgrades, w.upgradeFresh(observed, now))
		if ctx.Err() != nil {
			return
		}
	}
	var begun, ended, holds []state.Hold
	reconcile := func(st *state.State, at time.Time) ([]state.Event, bool) {
		begun, ended = nil, nil
		before := append([]state.Hold(nil), st.Holds...)
		var ev []state.Event
		for _, s := range statuses {
			if s.Err != "" {
				continue
			}
			b, e := upgrade.Reconcile(st, s.Installation, s.Upgrades, at, watchParty)
			for _, h := range b {
				ev = append(ev, event(watchParty, "hold.set", "%s until the upgrade ends: %s", h.Target, h.Reason))
			}
			for _, h := range e {
				ev = append(ev, event(watchParty, "hold.lift", "%s: the upgrade ended", h.Target))
			}
			begun, ended = append(begun, b...), append(ended, e...)
		}
		holds = append([]state.Hold(nil), st.Holds...)
		changed := len(ev) > 0 || len(before) != len(st.Holds)
		if !changed {
			for i := range before {
				if before[i].Target != st.Holds[i].Target || before[i].UpgradeTo != st.Holds[i].UpgradeTo {
					changed = true
					break
				}
			}
		}
		return ev, changed
	}
	needUpdate := true
	if w.a.store.Dir() != "" {
		current, readErr := w.a.store.Read()
		if readErr != nil {
			w.fail("upgrades", "UPGRADES unknown: the state does not load (%v)", readErr)
			return
		}
		_, needUpdate = reconcile(&state.State{Holds: append([]state.Hold(nil), current.Holds...)}, w.clk().Now())
	}
	var err error
	if needUpdate {
		err = w.a.store.Update(func(st *state.State) ([]state.Event, error) {
			now = w.clk().Now()
			if w.a.store.Dir() == "" {
				// Remote stores have no shared filesystem lock; retain observation
				// on each resource-version retry instead of reusing a stale reading.
				statuses = read(ctx, st, now, w.upgrades, w.upgradeFresh(st, now))
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			ev, _ := reconcile(st, now)
			return ev, nil
		})
	}
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		w.fail("upgrades", "UPGRADES unknown: the state does not load (%v)", err)
		return
	}
	w.clear("upgrades")
	if w.upgradeFailures == nil {
		w.upgradeFailures = map[string]int{}
	}
	var noContext []string
	for _, s := range statuses {
		if s.Err != "" {
			w.upgradeFailures[s.Installation]++
			for _, h := range holds {
				if h.Active(now) && strings.HasPrefix(h.Target, upgrade.HoldPrefix+s.Installation+"/") {
					w.emitNow("upgrade-reader", "%s held: reader failed %d consecutive ticks: %s", h.Target, w.upgradeFailures[s.Installation], s.Err)
				}
			}
		} else {
			delete(w.upgradeFailures, s.Installation)
		}
		switch {
		case s.NoContext:
			noContext = append(noContext, s.Installation)
			w.clear("upgrades:" + s.Installation)
		case s.Err != "":
			w.fail("upgrades:"+s.Installation, "UPGRADES %s unreadable: %s", s.Installation, s.Err)
		default:
			w.clear("upgrades:" + s.Installation)
		}
	}
	w.sayNoContext("UPGRADES", noContext)
	for _, h := range begun {
		w.emitNow("upgrade", "%s", upgrade.Line(h))
	}
	for _, h := range ended {
		w.emitNow("upgrade", "%s", upgrade.EndLine(h))
	}
	w.saveMark()
}

// sayNoContext says once per watch, in one line, the installations of the
// reader (ALERTS, UPGRADES) without a kube context: they are not read, and
// no line names them one by one.
func (w *watcher) sayNoContext(reader string, names []string) {
	if len(names) == 0 {
		return
	}
	w.mu.Lock()
	said := w.noContextSaid[reader]
	if w.noContextSaid == nil {
		w.noContextSaid = map[string]bool{}
	}
	w.noContextSaid[reader] = true
	w.mu.Unlock()
	if said {
		return
	}
	w.emitNow("nocontext", "%s: %s", reader, alerts.NoContextLine(names))
}
