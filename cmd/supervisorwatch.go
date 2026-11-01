package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/alerts"
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
	lock := flock.New(filepath.Join(w.store.Dir(), loopLockFile))
	held, err := lock.TryLock()
	if err != nil {
		return false
	}
	if held {
		_ = lock.Unlock()
	}
	return !held
}

func (w *watcher) publishAlerts(firing []alerts.Firing) {
	rules := w.alertRules()
	rules.InPlay = w.inPlay(time.Now())
	firing = slices.DeleteFunc(slices.Clone(firing), func(f alerts.Firing) bool { return !rules.Wakes(f.Installation, f.Alert) })
	raw, err := json.Marshal(firing)
	if err == nil && w.store != nil {
		_ = w.store.Record(event(watchParty, "watch.alert-state", "%s", raw))
	}
}

func (w *watcher) publishBudget(held bool) {
	if w.store != nil {
		_ = w.store.Record(event(watchParty, "watch.budget-state", "%t", held))
	}
}

// A fresh level also clears conditions left by a previous watch whose
// caller had a different mark file. Transition-only replay cannot do that.
func (w *watcher) publishMachine() {
	w.mu.Lock()
	active := map[string]string{}
	for _, key := range []string{loopDiskKind, "diskfull", "diskfill", "load", "cpupsi", "avail", "psi", "swap", "oomd"} {
		if c, ok := w.active[key]; ok {
			active[key] = c.Label
		}
	}
	w.mu.Unlock()
	raw, err := json.Marshal(active)
	if err == nil && w.store != nil {
		_ = w.store.Record(event(watchParty, "watch.machine-state", "%s", raw))
	}
}

type watchDiskReading struct {
	Low     bool
	Writers string
}

func (w *watcher) publishDisk(low bool, writers string) {
	raw, err := json.Marshal(watchDiskReading{Low: low, Writers: writers})
	if err == nil && w.store != nil {
		_ = w.store.Record(event(watchParty, "watch.disk-state", "%s", raw))
	}
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
	var budgetAt time.Time
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
			active = map[string]string{}
			if err := json.Unmarshal([]byte(e.Detail), &active); err != nil {
				return err
			}
		case "watch.clear":
			delete(active, e.Detail)
		case "watch.disk", "watch.diskfull", "watch.diskfill", "watch.load", "watch.cpupsi", "watch.avail", "watch.psi", "watch.swap", "watch.oomd":
			active[e.Verb[6:]] = e.Detail
		}
	}
	if disk.Low {
		active[loopDiskKind] = disk.Writers
		act := superloop.Action{Kind: loopDiskKind, Key: diskKey}
		if at, ok := pruned[diskKey]; !ok {
			claim, claimed := claims[diskKey]
			if !claimed || lastPrune.Before(claim) {
				if err := l.claim(act, "disk prune"); err != nil {
					return err
				}
				if _, err := l.run(ctx, loopDiskKind, "prune", "--apply"); err != nil {
					return err
				}
			}
			if err := l.record(act, "pruned"); err != nil {
				return err
			}
		} else if diskAt.After(at) && !noted[diskKey] {
			if l.a.cfg.Guide.Person == "" {
				return fmt.Errorf("guide.person is unset: cannot report low disk")
			}
			if err := l.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.NextNote++
				text := "Disk remains low after pruning; starts are held. Writers: " + disk.Writers
				st.Notes = append(st.Notes, state.Note{ID: st.NextNote, For: l.a.cfg.Guide.Person, By: loopParty, At: l.a.now, Text: text, Kind: noteDecision, Question: "Investigate remaining disk writers (no decision)", StatusQuo: text, Default: "the guide investigates the writers and reclaims disposable data; keep starts held until the watch reports disk recovery"})
				return []state.Event{event(loopParty, "note.add", "#%d %s", st.NextNote, text), event(loopParty, "supervisor.disk", "%s", superloop.Detail(act, "noted"))}, nil
			}); err != nil {
				return err
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
