package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

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
	if err := r.a.store.WriteFile("supervisor-loop-pid.json", os.Getpid()); err != nil {
		t.Fatal(err)
	}
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

func TestLoopWatchDiskFailureKeepsPassAlive(t *testing.T) {
	for _, missingGuide := range []bool{false, true} {
		t.Run(fmt.Sprint(missingGuide), func(t *testing.T) {
			r := newLoopRig(t)
			if missingGuide {
				r.a.cfg.Guide.Person = ""
			}
			prunes := 0
			r.answer = func(args []string) (string, int) {
				if len(args) > 1 && args[0] == loopDiskKind && args[1] == "prune" {
					prunes++
					if !missingGuide {
						return "partial prune failed", 1
					}
				}
				return "", 0
			}
			w := r.a.newWatcher(false, false)
			w.publishDisk(watchDiskReading{Low: true, Writers: "fixture"})
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if err := l.pass(context.Background()); err != nil {
					t.Fatalf("maintenance stopped pass: %v", err)
				}
				if !l.startsHeld {
					t.Fatal("low disk allowed starts")
				}
				w.publishDisk(watchDiskReading{Low: true, Writers: "fixture"})
			}
			restarted, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !restarted.startsHeld {
				t.Fatal("restart lost disk hold")
			}
			if prunes != 1 {
				t.Fatalf("prune attempts = %d, want 1", prunes)
			}
			if !strings.Contains(r.out.String(), "SUPERVISOR DISK") {
				t.Fatal("maintenance failure not reported")
			}
		})
	}
}

func TestLoopWatchLevelsOnlyLogTransitions(t *testing.T) {
	r := newLoopRig(t)
	w := r.a.newWatcher(false, false)
	for i := range 10 {
		w.publishBudget(false)
		w.publishMachine()
		w.publishAlerts(nil)
		w.publishDisk(watchDiskReading{Low: true, FreeMiB: i, MinMiB: 100})
	}
	evs, err := r.a.store.Events(0, func(e state.Event) bool { return strings.HasSuffix(e.Verb, "-state") })
	if err != nil {
		t.Fatal(err)
	}
	// One legacy budget seed from the rig, then four initial transitions.
	if len(evs) != 5 {
		t.Fatalf("repeated levels appended %d events, want 5", len(evs))
	}
	if got := r.a.watchDisk(); got == nil || got.FreeMiB != 9 {
		t.Fatalf("latest level missing: %+v", got)
	}
}

func TestLoopWatchDiskNoteFailureKeepsPassAlive(t *testing.T) {
	r := newLoopRig(t)
	w := r.a.newWatcher(false, false)
	w.publishDisk(watchDiskReading{Low: true})
	r.pass(t)
	w.publishDisk(watchDiskReading{Low: true})
	r.failRecord = func(e state.Event) bool { return e.Verb == "note.add" }
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !l.startsHeld {
		t.Fatal("failed note cleared hold")
	}
	if !strings.Contains(r.out.String(), "note failed") {
		t.Fatal("note failure not reported")
	}
	if err := l.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ := r.a.store.Read()
	if len(st.Notes) != 1 {
		t.Fatalf("note retry: %+v", st.Notes)
	}
}
