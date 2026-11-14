package cmd

import (
	"bytes"
	"encoding/json"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/capacity"
	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/proc"
)

func (w *Watcher) LoopRunning() bool {
	if w.LoopOwned {
		return true
	}
	var pid int
	found, err := w.Host.store.ReadFile(loop.LoopPIDFile, &pid)
	return err == nil && found && pid > 0 && proc.Alive(pid)
}

// Current levels live in bounded side files; only changes enter the audit log.
// Keep event reads as a migration fallback for watches from older binaries.
type watchLevel struct {
	At    time.Time
	Value json.RawMessage
}

func (w *Watcher) publishLevel(kind string, value any) {
	if w.Host.store == nil {
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	name := "watch-" + kind + ".json"
	var old watchLevel
	found, err := w.Host.store.ReadFile(name, &old)
	if err != nil {
		return
	}
	next := watchLevel{At: clk.Now(), Value: raw}
	if err := w.Host.store.WriteFile(name, next); err != nil {
		return
	}
	changed := !bytes.Equal(old.Value, raw)
	if kind == "disk-state" && found {
		var before, after capacity.WatchDiskReading
		if json.Unmarshal(old.Value, &before) == nil && json.Unmarshal(raw, &after) == nil {
			changed = before.Low != after.Low
		}
	}
	if !found || changed {
		_ = w.Host.store.Record(event(WatchParty, "watch."+kind, "%s", raw))
	}
}

func (w *Watcher) PublishAlerts(firing []alerts.Firing) {
	rules := w.Host.alertRules()
	rules.InPlay = w.Host.inPlay(clk.Now())
	firing = slices.DeleteFunc(slices.Clone(firing), func(f alerts.Firing) bool { return !rules.Wakes(f.Installation, f.Alert) })
	w.publishLevel("alert-state", firing)
}

func (w *Watcher) PublishBudget(held bool) { w.publishLevel("budget-state", held) }

func (w *Watcher) PublishMachine() {
	w.Mu.Lock()
	active := map[string]string{}
	// diskfill is absent on purpose: DISK FILLING is a forecast from the
	// write rate, and a forecast never holds starts; only the disk-state
	// reading's Low (free space under watch.diskMinMiB) does.
	for _, key := range []string{loop.LoopDiskKind, "diskfull", "load", "cpupsi", "avail", "psi", "swap", "oomd"} {
		if c, ok := w.Active[key]; ok {
			active[key] = c.Label
		}
	}
	w.Mu.Unlock()
	w.publishLevel("machine-state", active)
}

func (w *Watcher) PublishDisk(reading capacity.WatchDiskReading) {
	w.publishLevel("disk-state", reading)
}
