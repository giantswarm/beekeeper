package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The installed loop owns a watch for its entire lifetime. Use a separate
// app value: both loops update their clock and transcript cache.
var runLoopWatch = func(ctx context.Context, w *Watcher) error { return w.Run(ctx, false) }

func (a *app) startLoopWatch(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	observing := *a.snapshotAtNow()
	cfg := *a.cfg
	cfg.Watch.BudgetEvery.Duration = max(30*time.Second, cfg.Watch.Interval.Duration)
	observing.cfg = &cfg
	w := observing.newWatcher(false, true)
	w.LoopOwned = true
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer w.Bg.Wait()
		defer w.Stand.Inflight.Wait()
		defer w.TimerActs.Wait()
		if err := runLoopWatch(ctx, w); err != nil && ctx.Err() == nil {
			_, _ = fmt.Fprintln(a.out, "SUPERVISOR WATCH FAILED:", err)
		}
	}()
	return func() { cancel(); <-done }
}

func (a *app) readWatchLevel(kind string, value any) (time.Time, error) {
	var level watchLevel
	found, err := a.store.ReadFile("watch-"+kind+".json", &level)
	if err != nil || !found {
		return time.Time{}, err
	}
	return level.At, json.Unmarshal(level.Value, value)
}

func (a *app) watchDisk() *watchDiskReading {
	if a.store == nil {
		return nil
	}
	var current watchDiskReading
	if at, err := a.readWatchLevel("disk-state", &current); err == nil && !at.IsZero() {
		return &current
	}
	evs, err := state.FollowLast(a.store, "watch-disk-state", 1, func(e state.Event) bool { return e.Verb == loop.WatchDiskStateVerb })
	if err != nil || len(evs) == 0 {
		return nil
	}
	var reading watchDiskReading
	if err := json.Unmarshal([]byte(evs[0].Detail), &reading); err != nil {
		return nil
	}
	return &reading
}
