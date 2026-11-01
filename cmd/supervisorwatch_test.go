package cmd

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestLoopWatchKeepsQuietAlertsOutOfEventFeed(t *testing.T) {
	read := readLoopAlerts
	r := newLoopRig(t)
	r.a.cfg.Alerts.Quiet = []alerts.Quiet{{Cluster: "test-*"}}
	w := r.a.newWatcher(false, false)
	f := loopFiring()
	f.Cluster = "test-cluster"
	w.publishAlerts([]alerts.Firing{f})
	got, err := read(context.Background(), r.a)
	if err != nil || len(got) != 0 {
		t.Fatalf("quiet alert entered event feed: %+v %v", got, err)
	}
}

func TestLoopOwnsWatchUntilShutdown(t *testing.T) {
	r := newLoopRig(t)
	old := runLoopWatch
	t.Cleanup(func() { runLoopWatch = old })
	started, ended := make(chan *watcher, 1), make(chan struct{})
	runLoopWatch = func(ctx context.Context, w *watcher) error {
		started <- w
		<-ctx.Done()
		close(ended)
		return nil
	}
	stop := r.a.startLoopWatch(context.Background())
	w := <-started
	if w.standby || !w.loopOwned || w.cfg.Watch.BudgetEvery != w.cfg.Watch.Interval {
		t.Error("watch does not own alert and per-poll budget observations")
	}
	stop()
	select {
	case <-ended:
	default:
		t.Fatal("watch outlived loop shutdown")
	}
}

func TestWatchRecognizesLoopWithoutSupervisorSession(t *testing.T) {
	r := newLoopRig(t)
	w := r.a.newWatcher(true, false)
	lock := flock.New(filepath.Join(r.a.store.Dir(), loopLockFile))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Unlock() }()
	if !w.supervisorGone(context.Background(), &state.State{}, nil) {
		t.Fatal("standby ignored running loop")
	}
}

func TestLoopWatchPrunesThenReportsPersistentLowDiskOnce(t *testing.T) {
	r := newLoopRig(t)
	w := r.a.newWatcher(false, false)
	w.publishDisk(watchDiskReading{Low: true, Writers: "test-writer"})
	r.pass(t)
	if r.called(loopDiskKind, "prune", "--apply") == nil {
		t.Fatal("low disk not pruned")
	}
	w.publishDisk(watchDiskReading{Low: true, Writers: "test-writer"})
	r.pass(t)
	if r.called(loopDiskKind, "prune", "--apply") != nil {
		t.Fatal("same disk episode pruned twice")
	}
	st, _ := r.a.store.Read()
	if len(st.Notes) != 1 || !strings.Contains(st.Notes[0].Text, "test-writer") {
		t.Fatalf("persistent low disk note: %+v", st.Notes)
	}
	r.pass(t)
	st, _ = r.a.store.Read()
	if len(st.Notes) != 1 {
		t.Fatal("duplicate disk note across restart")
	}
}

func TestLoopWatchPressureHoldsAndRecovers(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	w := r.a.newWatcher(false, false)
	w.check("load", true, "HIGH LOAD: test")
	r.pass(t)
	if r.called(agentsName, agentStartName) != nil {
		t.Fatal("start during high load")
	}
	if !strings.Contains(r.out.String(), "SUPERVISOR STARTS HELD") {
		t.Fatal("hold not explained")
	}
	w.clear("load")
	r.pass(t)
	if r.called(agentsName, agentStartName) == nil {
		t.Fatal("start did not recover")
	}
}

func TestLoopWatchStaleBudgetHoldsStarts(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	r.a.now = r.a.now.Add(time.Hour)
	if err := l.watchConditions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !l.startsHeld {
		t.Fatal("stale budget allowed new starts")
	}
}

func TestLoopWatchDiskFeedsCapacity(t *testing.T) {
	r := newLoopRig(t)
	w := r.a.newWatcher(false, false)
	w.publishDisk(watchDiskReading{Low: true, FreeMiB: 1, MinMiB: 100})
	got := r.a.watchDisk()
	if got == nil || !got.Low || got.FreeMiB != 1 || got.MinMiB != 100 {
		t.Fatalf("capacity disk reading: %+v", got)
	}
}

func TestLoopWatchFreshMachineReadingClearsPreviousWatchPressure(t *testing.T) {
	r := newLoopRig(t)
	old := r.a.newWatcher(false, false)
	old.check("load", true, "HIGH LOAD: old watch")
	fresh := r.a.newWatcher(false, false)
	fresh.publishMachine()
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.watchConditions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.startsHeld {
		t.Fatal("fresh healthy watch did not clear old pressure")
	}
}
