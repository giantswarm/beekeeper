package cmd

import (
	"context"
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
	if w.standby || !w.loopOwned || w.cfg.Watch.BudgetEvery.Duration != max(30*time.Second, w.cfg.Watch.Interval.Duration) {
		t.Error("watch does not own alert and bounded budget observations")
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
	if err := r.a.store.WriteFile(LoopPIDFile, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if !w.checkLoop(context.Background(), &state.State{}) {
		t.Fatal("standby ignored running loop")
	}
}

func TestLoopWatchFillForecastReleasesStartsWhenItStops(t *testing.T) {
	r := newLoopRig(t)
	w := r.a.newWatcher(false, false)
	w.publishDisk(watchDiskReading{Low: true, Filling: true, Writers: "go build", FreeMiB: 1 * gibMiB, MinMiB: 18 * gibMiB})
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.Once = true // These checks inspect completed pass effects.
	if err := l.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !l.StartsHeld {
		t.Fatal("low disk allowed starts")
	}
	// The writer finishes; the rate falls and the prune frees enough: the
	// recovery in the reading lifts the hold on its own, no note closes,
	// no person acts.
	w.publishDisk(watchDiskReading{Low: false, Filling: false, Writers: stateIdle, FreeMiB: 25 * gibMiB, MinMiB: 18 * gibMiB})
	if err := l.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.StartsHeld {
		t.Fatal("the hold outlived the recovery")
	}
}

func TestLoopWatchPrunesThenFilesPersistentLowDiskOnce(t *testing.T) {
	r := newLoopRig(t)
	w := r.a.newWatcher(false, false)
	w.publishDisk(watchDiskReading{Low: true, Writers: "test-writer"})
	r.pass(t)
	if r.called(LoopDiskKind, "prune", "--apply") == nil {
		t.Fatal("low disk not pruned")
	}
	// The prune's pressure run recorded what still holds the space.
	if err := r.a.store.WriteFile(diskHoldingFile, diskHolding{At: r.a.now, Text: "caches app 9 GiB (last write 2026-10-27)"}); err != nil {
		t.Fatal(err)
	}
	w.publishDisk(watchDiskReading{Low: true, Writers: "test-writer"})
	r.pass(t)
	if r.called(LoopDiskKind, "prune", "--apply") != nil {
		t.Fatal("same disk episode pruned twice")
	}
	st, _ := r.a.store.Read()
	if len(st.Notes) != 0 {
		t.Fatalf("low disk filed a note for the person: %+v", st.Notes)
	}
	evs, err := r.a.store.Events(0, func(e state.Event) bool { return e.Verb == LoopFileVerb })
	if err != nil || len(evs) != 1 {
		t.Fatalf("persistent low disk findings: %+v %v", evs, err)
	}
	if !strings.Contains(evs[0].Detail, "test-writer") || !strings.Contains(evs[0].Detail, "Holding the space: caches app 9 GiB") ||
		!strings.Contains(evs[0].Detail, FileReportSeparator) {
		t.Fatalf("persistent low disk finding: %q", evs[0].Detail)
	}
	lines, changed := guideProblemLines(&state.State{}, evs)
	if !changed || len(lines) != 1 || !strings.HasPrefix(lines[0], "GUIDE PROBLEM: ") || !strings.Contains(lines[0], "every worker start is held") || !strings.Contains(lines[0], "floor") {
		t.Fatalf("low disk did not raise a guide alarm: %v", lines)
	}

	r.pass(t)
	evs, err = r.a.store.Events(0, func(e state.Event) bool { return e.Verb == LoopFileVerb })
	if err != nil || len(evs) != 1 {
		t.Fatalf("duplicate disk finding across restart: %+v %v", evs, err)
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
	l.Once = true // These checks inspect completed pass effects.
	r.a.now = r.a.now.Add(time.Hour)
	if err := l.WatchConditions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !l.StartsHeld {
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
	l.Once = true // These checks inspect completed pass effects.
	if err := l.WatchConditions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.StartsHeld {
		t.Fatal("fresh healthy watch did not clear old pressure")
	}
}

func TestLoopWatchDiskFailureKeepsPassAlive(t *testing.T) {
	r := newLoopRig(t)
	prunes := 0
	r.answer = func(args []string) (string, int) {
		if len(args) > 1 && args[0] == LoopDiskKind && args[1] == litPrune {
			prunes++
			return "partial prune failed", 1
		}
		return "", 0
	}
	w := r.a.newWatcher(false, false)
	w.publishDisk(watchDiskReading{Low: true, Writers: loopWorker})
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.Once = true // These checks inspect completed pass effects.
	for range 3 {
		if err := l.Pass(context.Background()); err != nil {
			t.Fatalf("maintenance stopped pass: %v", err)
		}
		if !l.StartsHeld {
			t.Fatal("low disk allowed starts")
		}
		w.publishDisk(watchDiskReading{Low: true, Writers: loopWorker})
	}
	restarted, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	restarted.Once = true // These checks inspect completed pass effects.
	if err := restarted.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !restarted.StartsHeld {
		t.Fatal("restart lost disk hold")
	}
	if prunes != 1 {
		t.Fatalf("prune attempts = %d, want 1", prunes)
	}
	if !strings.Contains(r.out.String(), "SUPERVISOR DISK") {
		t.Fatal("maintenance failure not reported")
	}
}

// A failed cleanup's cause must survive the loop restarting before the next
// disk sample: the guide needs the cause, not just the usage-level hold.
func TestLoopWatchDiskFailureReachesGuideAfterRestart(t *testing.T) {
	r := newLoopRig(t)
	r.answer = func(args []string) (string, int) {
		if len(args) > 1 && args[0] == LoopDiskKind && args[1] == litPrune {
			return "cleanup unavailable", 1
		}
		return "", 0
	}
	w := r.a.newWatcher(false, false)
	w.publishDisk(watchDiskReading{Low: true, FreeMiB: 1, MinMiB: 100})
	r.pass(t)
	if findings := r.logged(t, LoopFileVerb); len(findings) != 0 {
		t.Fatalf("alarm before a post-cleanup sample: %+v", findings)
	}
	w.publishDisk(watchDiskReading{Low: true, FreeMiB: 2, MinMiB: 100})
	r.pass(t) // Each rig pass creates a fresh supervisor loop.
	findings := r.logged(t, LoopFileVerb)
	if len(findings) != 1 {
		t.Fatalf("disk findings: %+v", findings)
	}
	for _, want := range []string{"P1:", "every worker start is held", "2 MiB (floor 100 MiB)", "Cleanup failed:", "cleanup unavailable"} {
		if !strings.Contains(findings[0].Detail, want) {
			t.Errorf("finding missing %q: %s", want, findings[0].Detail)
		}
	}
	lines, changed := guideProblemLines(&state.State{}, findings)
	if !changed || len(lines) != 1 || !strings.HasPrefix(lines[0], "GUIDE PROBLEM: ") || !strings.Contains(lines[0], "cleanup unavailable") {
		t.Fatalf("cleanup failure did not reach guide: %v", lines)
	}
	r.pass(t)
	if findings := r.logged(t, LoopFileVerb); len(findings) != 1 {
		t.Fatalf("duplicate failure alarm: %+v", findings)
	}
	w.publishDisk(watchDiskReading{FreeMiB: 200, MinMiB: 100})
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.Once = true
	if err := l.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.StartsHeld {
		t.Fatal("cleanup failure prevented automatic recovery")
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

func TestLoopWatchDiskFindingFailureKeepsPassAlive(t *testing.T) {
	r := newLoopRig(t)
	w := r.a.newWatcher(false, false)
	w.publishDisk(watchDiskReading{Low: true})
	r.pass(t)
	w.publishDisk(watchDiskReading{Low: true})
	r.failRecord = func(e state.Event) bool { return e.Verb == LoopFileVerb }
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.Once = true // These checks inspect completed pass effects.
	if err := l.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !l.StartsHeld {
		t.Fatal("failed finding cleared hold")
	}
	if !strings.Contains(r.out.String(), "finding failed") {
		t.Fatal("finding failure not reported")
	}
	if err := l.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	evs, err := r.a.store.Events(0, func(e state.Event) bool { return e.Verb == LoopFileVerb })
	if err != nil || len(evs) != 1 {
		t.Fatalf("finding retry: %+v %v", evs, err)
	}
	st, _ := r.a.store.Read()
	if len(st.Notes) != 0 {
		t.Fatalf("low disk filed a note for the person: %+v", st.Notes)
	}
}

func TestLoopWatchDiskRecoveryAfterPruneDoesNotAlarm(t *testing.T) {
	r := newLoopRig(t)
	w := r.a.newWatcher(false, false)
	w.publishDisk(watchDiskReading{Low: true, FreeMiB: 1, MinMiB: 100})
	r.pass(t)
	if r.called(LoopDiskKind, "prune", "--apply") == nil {
		t.Fatal("disk floor did not trigger cleanup")
	}
	w.publishDisk(watchDiskReading{FreeMiB: 200, MinMiB: 100})
	r.pass(t)
	events, err := r.a.store.Events(0, func(e state.Event) bool { return e.Verb == LoopFileVerb })
	if err != nil || len(events) != 0 {
		t.Fatalf("recovered disk raised an alarm: %+v %v", events, err)
	}
	if reading := r.a.watchDisk(); reading == nil || reading.Low {
		t.Fatalf("recovered disk still blocks capacity: %+v", reading)
	}
}
