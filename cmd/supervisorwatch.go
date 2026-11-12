package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The installed loop owns a watch for its entire lifetime. Use a separate
// app value: both loops update their clock and transcript cache.
var runLoopWatch = func(ctx context.Context, w *watcher) error { return w.run(ctx, false) }

func (a *app) startLoopWatch(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	observing := *a.snapshotAtNow()
	cfg := *a.cfg
	cfg.Watch.BudgetEvery.Duration = max(30*time.Second, cfg.Watch.Interval.Duration)
	observing.cfg = &cfg
	w := observing.newWatcher(false, true)
	w.loopOwned = true
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer w.bg.Wait()
		defer w.stand.inflight.Wait()
		defer w.timerActs.Wait()
		if err := runLoopWatch(ctx, w); err != nil && ctx.Err() == nil {
			_, _ = fmt.Fprintln(a.out, "SUPERVISOR WATCH FAILED:", err)
		}
	}()
	return func() { cancel(); <-done }
}

func (w *watcher) loopRunning() bool {
	if w.loopOwned {
		return true
	}
	var pid int
	found, err := w.store.ReadFile(loopPIDFile, &pid)
	return err == nil && found && pid > 0 && proc.Alive(pid)
}

// Current levels live in bounded side files; only changes enter the audit log.
// Keep event reads as a migration fallback for watches from older binaries.
type watchLevel struct {
	At    time.Time
	Value json.RawMessage
}

func (w *watcher) publishLevel(kind string, value any) {
	if w.store == nil {
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	name := "watch-" + kind + ".json"
	var old watchLevel
	found, err := w.store.ReadFile(name, &old)
	if err != nil {
		return
	}
	next := watchLevel{At: clk.Now(), Value: raw}
	if err := w.store.WriteFile(name, next); err != nil {
		return
	}
	changed := !bytes.Equal(old.Value, raw)
	if kind == "disk-state" && found {
		var before, after watchDiskReading
		if json.Unmarshal(old.Value, &before) == nil && json.Unmarshal(raw, &after) == nil {
			changed = before.Low != after.Low
		}
	}
	if !found || changed {
		_ = w.store.Record(event(watchParty, "watch."+kind, "%s", raw))
	}
}

func (a *app) readWatchLevel(kind string, value any) (time.Time, error) {
	var level watchLevel
	found, err := a.store.ReadFile("watch-"+kind+".json", &level)
	if err != nil || !found {
		return time.Time{}, err
	}
	return level.At, json.Unmarshal(level.Value, value)
}

func (w *watcher) publishAlerts(firing []alerts.Firing) {
	rules := w.alertRules()
	rules.InPlay = w.inPlay(clk.Now())
	firing = slices.DeleteFunc(slices.Clone(firing), func(f alerts.Firing) bool { return !rules.Wakes(f.Installation, f.Alert) })
	w.publishLevel("alert-state", firing)
}

func (w *watcher) publishBudget(held bool) { w.publishLevel("budget-state", held) }

func (w *watcher) publishMachine() {
	w.mu.Lock()
	active := map[string]string{}
	// diskfill is absent on purpose: DISK FILLING is a forecast from the
	// write rate, and a forecast never holds starts; only the disk-state
	// reading's Low (free space under watch.diskMinMiB) does.
	for _, key := range []string{loopDiskKind, "diskfull", "load", "cpupsi", "avail", "psi", "swap", "oomd"} {
		if c, ok := w.active[key]; ok {
			active[key] = c.Label
		}
	}
	w.mu.Unlock()
	w.publishLevel("machine-state", active)
}

func (w *watcher) publishDisk(reading watchDiskReading) { w.publishLevel("disk-state", reading) }

func (a *app) watchDisk() *watchDiskReading {
	if a.store == nil {
		return nil
	}
	var current watchDiskReading
	if at, err := a.readWatchLevel("disk-state", &current); err == nil && !at.IsZero() {
		return &current
	}
	evs, err := state.FollowLast(a.store, "watch-disk-state", 1, func(e state.Event) bool { return e.Verb == watchDiskStateVerb })
	if err != nil || len(evs) == 0 {
		return nil
	}
	var reading watchDiskReading
	if err := json.Unmarshal([]byte(evs[0].Detail), &reading); err != nil {
		return nil
	}
	return &reading
}
