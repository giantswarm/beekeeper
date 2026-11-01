package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const loopDiskKind = "disk"

// The installed loop owns a watch for its entire lifetime. Use a separate
// app value: both loops update their clock and transcript cache.
var runLoopWatch = func(ctx context.Context, w *watcher) error { return w.run(ctx, false) }

func (a *app) startLoopWatch(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	observing := *a
	cfg := *a.cfg
	cfg.Watch.BudgetEvery = cfg.Watch.Interval
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
	found, err := w.store.ReadFile("supervisor-loop-pid.json", &pid)
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
	next := watchLevel{At: time.Now(), Value: raw}
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
	rules.InPlay = w.inPlay(time.Now())
	firing = slices.DeleteFunc(slices.Clone(firing), func(f alerts.Firing) bool { return !rules.Wakes(f.Installation, f.Alert) })
	w.publishLevel("alert-state", firing)
}

func (w *watcher) publishBudget(held bool) { w.publishLevel("budget-state", held) }

func (w *watcher) publishMachine() {
	w.mu.Lock()
	active := map[string]string{}
	for _, key := range []string{loopDiskKind, "diskfull", "diskfill", "load", "cpupsi", "avail", "psi", "swap", "oomd"} {
		if c, ok := w.active[key]; ok {
			active[key] = c.Label
		}
	}
	w.mu.Unlock()
	w.publishLevel("machine-state", active)
}

type watchDiskReading struct {
	Low     bool
	Writers string
	FreeMiB int
	MinMiB  int
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
	evs, err := a.store.Events(1, func(e state.Event) bool { return e.Verb == "watch.disk-state" })
	if err != nil || len(evs) == 0 {
		return nil
	}
	var reading watchDiskReading
	if err := json.Unmarshal([]byte(evs[0].Detail), &reading); err != nil {
		return nil
	}
	return &reading
}

// Watch conditions are lasting levels, reconstructed after a restart.
// Budget readings expire rather than treating a dead watch as permission
// to start workers. Thresholds and all source reads stay in watch.
func (l *supervisorLoop) watchConditions(ctx context.Context) error {
	evs, err := l.a.store.Events(0, func(e state.Event) bool {
		return strings.HasPrefix(e.Verb, "watch.") || e.Verb == "supervisor.disk" || e.Verb == loopClaimVerb || e.Verb == "disk.prune"
	})
	if err != nil {
		return err
	}
	active := map[string]string{}
	var budgetAt, machineAt time.Time
	budgetHeld := true
	var disk watchDiskReading
	var diskKey string
	var diskAt time.Time
	pruned := map[string]time.Time{}
	noted := map[string]bool{}
	claims := map[string]time.Time{}
	var lastPrune time.Time
	for _, e := range evs {
		switch e.Verb {
		case loopClaimVerb:
			if key, target, ok := keyed(e.Detail); ok && target == "disk prune" {
				claims[key] = e.At
			}
		case "disk.prune":
			lastPrune = e.At
		case "watch.disk-state":
			if err := json.Unmarshal([]byte(e.Detail), &disk); err != nil {
				return err
			}
			diskAt = e.At
			if !disk.Low {
				diskKey = ""
			} else if diskKey == "" {
				diskKey = "disk " + e.At.UTC().Format(time.RFC3339Nano)
			}
		case "supervisor.disk":
			key, what, ok := keyed(e.Detail)
			if ok && what == "pruned" {
				pruned[key] = e.At
			}
			if ok && what == "noted" {
				noted[key] = true
			}
		case "watch.budget-state":
			budgetAt, budgetHeld = e.At, e.Detail != "false"
		case "watch.machine-state":
			machineAt = e.At
			active = map[string]string{}
			if err := json.Unmarshal([]byte(e.Detail), &active); err != nil {
				return err
			}
		case "watch.clear":
			machineAt = e.At
			delete(active, e.Detail)
		case "watch.disk", "watch.diskfull", "watch.diskfill", "watch.load", "watch.cpupsi", "watch.avail", "watch.psi", "watch.swap", "watch.oomd":
			machineAt = e.At
			active[e.Verb[6:]] = e.Detail
		}
	}
	// Prefer fresh levels without replaying identical samples into events.jsonl.
	var freshBudget bool
	if at, err := l.a.readWatchLevel("budget-state", &freshBudget); err != nil {
		return err
	} else if at.After(budgetAt) {
		budgetAt, budgetHeld = at, freshBudget
	}
	var freshMachine map[string]string
	if at, err := l.a.readWatchLevel("machine-state", &freshMachine); err != nil {
		return err
	} else if at.After(machineAt) {
		active = freshMachine
	}
	var freshDisk watchDiskReading
	if at, err := l.a.readWatchLevel("disk-state", &freshDisk); err != nil {
		return err
	} else if at.After(diskAt) {
		diskAt, disk = at, freshDisk
	}
	// Maintenance must never take the loop (and its watch) down. Establish
	// the hold before attempting any disk action, including failed writes.
	if disk.Low {
		l.startsHeld = true
		active[loopDiskKind] = disk.Writers
		act := superloop.Action{Kind: loopDiskKind, Key: diskKey}
		if at, ok := pruned[diskKey]; !ok {
			claim, claimed := claims[diskKey]
			if !claimed || lastPrune.Before(claim) {
				if err := l.claim(act, "disk prune"); err != nil {
					return err
				}
				if _, err := l.run(ctx, loopDiskKind, "prune", "--apply"); err != nil {
					l.sayOnce(diskKey+" prune", "SUPERVISOR DISK prune failed; starts remain held: %v", err)
				}
			}
			if err := l.record(act, "pruned"); err != nil {
				return err
			}
		} else if diskAt.After(at) && !noted[diskKey] {
			if l.a.cfg.Guide.Person == "" {
				l.sayOnce(diskKey+" note", "SUPERVISOR DISK guide.person is unset: cannot report low disk; starts remain held")
			} else if err := l.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.NextNote++
				text := "Disk remains low after pruning; starts are held. Writers: " + disk.Writers
				var holding diskHolding
				if found, _ := l.a.store.ReadFile(diskHoldingFile, &holding); found && holding.Text != "" && l.a.now.Sub(holding.At) < time.Hour {
					text += ". Holding the space: " + holding.Text
				}
				st.Notes = append(st.Notes, state.Note{ID: st.NextNote, For: l.a.cfg.Guide.Person, By: loopParty, At: l.a.now, Text: text, Kind: noteDecision, Question: "Investigate remaining disk writers (no decision)", StatusQuo: text, Default: "the guide investigates the writers and reclaims disposable data; keep starts held until the watch reports disk recovery"})
				return []state.Event{event(loopParty, "note.add", "#%d %s", st.NextNote, text), event(loopParty, "supervisor.disk", "%s", superloop.Detail(act, "noted"))}, nil
			}); err != nil {
				l.sayOnce(diskKey+" note", "SUPERVISOR DISK note failed; starts remain held: %v", err)
			}
		}
	}
	stale := budgetAt.IsZero() || l.a.now.Sub(budgetAt) > max(3*l.a.cfg.Watch.Interval.Duration, time.Minute)
	l.startsHeld = budgetHeld || stale || len(active) > 0
	if l.startsHeld {
		l.sayOnce("watch-hold", "SUPERVISOR STARTS HELD: watch budget held=%t stale=%t; machine conditions %v", budgetHeld, stale, active)
	} else {
		delete(l.said, "watch-hold")
	}
	return nil
}
