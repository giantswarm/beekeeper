package cmd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// Given an observation pass blocked reading a pull, when the independent
// refill clock ticks, then board workers start before that read is released.
// This is the in-process regression for the behavioral-scenario plan's 3636 case.
func TestRefillCadenceWhileObservationIsBlocked(t *testing.T) {
	r, busy := fillRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.IndependentFill = true
	r.park(t, loopPR, r.a.timeNow())
	entered, release := make(chan struct{}), make(chan struct{})
	original := loop.ReadLoopPull
	var first sync.Once
	loop.ReadLoopPull = func(ctx context.Context, ref string) (superloop.Pull, error) {
		first.Do(func() { close(entered); <-release })
		return original(ctx, ref)
	}
	passDone := make(chan error, 1)
	go func() { passDone <- l.Pass(t.Context()) }()
	defer func() {
		close(release)
		if err := <-passDone; err != nil {
			t.Error(err)
		}
		l.PassActions.Wait()
		l.MergeActions.Wait()
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("observation did not reach the blocked pull read")
	}
	ticks := make(chan time.Time)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); l.RunRefillCadence(ctx, ticks) }()
	select {
	case ticks <- r.a.timeNow():
	case <-ctx.Done():
		t.Fatal("refill clock did not progress")
	}
	// An unbuffered next tick synchronizes with completion of the first refill.
	select {
	case ticks <- r.a.timeNow():
	case <-ctx.Done():
		t.Fatal("refill clock did not progress")
	}
	cancel()
	<-done
	if *busy != 3 {
		t.Fatalf("blocked observation starved refill: %d workers", *busy)
	}
}

// Each held tick names its admission reason, even after the watch's first
// diagnostic. Once admission recovers, the tick fills all free slots and
// must not also report zero starts.
func TestRefillCadenceHonoursHoldAndRecovers(t *testing.T) {
	r, busy := fillRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	w := r.a.newWatcher(false, false)
	w.publishBudget(true)
	for range 2 {
		if err := l.RefillTick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if *busy != 0 {
		t.Fatal("started through a hold")
	}
	if got := strings.Count(r.out.String(), refillZeroLine+"watch budget held=true stale=false"); got != 2 {
		t.Fatalf("want one budget explanation per tick, got %d: %s", got, r.out)
	}
	if got := strings.Count(r.out.String(), "SUPERVISOR STARTS HELD:"); got != 1 {
		t.Fatalf("watch transition diagnostic repeated %d times: %s", got, r.out)
	}
	r.out.Reset()
	w.publishBudget(false)
	if err := l.RefillTick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *busy != 3 {
		t.Fatalf("did not recover: %d workers", *busy)
	}
	if strings.Contains(r.out.String(), refillZeroLine) {
		t.Fatalf("reported zero after starting workers: %s", r.out)
	}
}

func TestRefillStallNamesActiveAction(t *testing.T) {
	r, _ := fillRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.FillRan()
	l.SetFillPhase("board next")
	r.a.now = r.a.now.Add(loop.LoopStallAfter)
	if err := l.CheckStall(); err != nil {
		t.Fatal(err)
	}
	events := r.logged(t, "supervisor.file")
	if len(events) != 1 || !strings.Contains(events[0].Detail, "board next") {
		t.Fatalf("stall did not identify the blocked action: %+v", events)
	}
}

// A background context already owned by observation must not hide cancellation
// of the cadence itself: shutdown joins the active refill before closing state.
func TestRefillCadenceCancelsActiveTick(t *testing.T) {
	r, _ := fillRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.BackgroundContext(t.Context())
	defer l.CancelBackgroundActions()
	original := loop.BeekeeperRun
	entered := make(chan struct{})
	loop.BeekeeperRun = func(ctx context.Context, args ...string) (string, int, error) {
		if strings.Contains(strings.Join(args, " "), "capacity --json") {
			close(entered)
			<-ctx.Done()
			return "", 1, ctx.Err()
		}
		return original(ctx, args...)
	}
	ticks := make(chan time.Time, 1)
	ticks <- r.a.timeNow()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); l.RunRefillCadence(ctx, ticks) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refill never entered capacity")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cadence cancellation did not stop the active refill")
	}
	if l.ReexecBusy() {
		t.Fatal("cancelled refill retained its in-flight reservation")
	}
}

func TestRefillCadenceDoesNotOverlap(t *testing.T) {
	r, busy := fillRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.BeginInFlight(loop.LoopFillStream)
	defer l.EndInFlight(loop.LoopFillStream)
	if err := l.RefillTick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *busy != 0 {
		t.Fatal("started a second refill while the first was active")
	}
}

// Production agents.start events must not make completed claims look interrupted.
func TestRefillCadenceDoesNotReplayCompletedStarts(t *testing.T) {
	r, busy := fillRig(t)
	original := r.answer
	r.answer = func(args []string) (string, int) {
		out, code := original(args)
		if args[0] == agentsName && args[1] == agentStartName {
			if err := r.a.store.Record(event(loop.LoopParty, verbStart, "%s: started", args[2])); err != nil {
				t.Error(err)
			}
		}
		return out, code
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.Lifecycle = &loop.LoopLifecycle{Phase: "blocked observation", At: r.a.timeNow()}
	before := *l.Lifecycle
	for range 3 {
		if err := l.RefillTick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if *busy != 3 || len(r.logged(t, "supervisor.next")) != 3 {
		t.Fatalf("replayed completed starts: busy=%d, events=%+v", *busy, r.logged(t, "supervisor.next"))
	}
	if l.Lifecycle.Phase != before.Phase || !l.Lifecycle.At.Equal(before.At) {
		t.Fatal("refill refreshed a blocked observation heartbeat")
	}
}

func TestRefillCadenceRefreshesWatchWithoutPass(t *testing.T) {
	for _, condition := range []string{fatalBudgetCommand, secLoad, loop.LoopDiskKind, kindStale} {
		t.Run(condition, func(t *testing.T) {
			r, busy := fillRig(t)
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			l.IndependentFill = true
			w := r.a.newWatcher(false, false)
			switch condition {
			case fatalBudgetCommand:
				w.publishBudget(true)
			case secLoad:
				w.check(secLoad, true, "HIGH LOAD")
			case loop.LoopDiskKind:
				w.publishDisk(watchDiskReading{Low: true, FreeMiB: 1, MinMiB: 100})
			case kindStale:
				r.a.now = r.a.now.Add(time.Hour)
			}
			if err := l.RefillTick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if *busy != 0 {
				t.Fatal("started through a newly published hold")
			}
			switch condition {
			case fatalBudgetCommand:
				w.publishBudget(false)
			case secLoad:
				w.clear(secLoad)
			case loop.LoopDiskKind:
				w.publishDisk(watchDiskReading{FreeMiB: 200, MinMiB: 100})
			case kindStale:
				e := event(watchParty, "watch.budget-state", "false")
				e.At = r.a.timeNow()
				if err := r.a.store.Record(e); err != nil {
					t.Fatal(err)
				}
			}
			if err := l.RefillTick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if *busy != 3 {
				t.Fatalf("hold recovery waited for observation pass: %d", *busy)
			}
		})
	}
}

func TestRefillCadenceStartsBeforeFirstTick(t *testing.T) {
	r, busy := fillRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.HoldStarts(true)
	// A closed clock terminates after the startup refill without supplying a tick.
	ticks := make(chan time.Time)
	close(ticks)
	l.RunRefillCadence(t.Context(), ticks)
	defer l.CancelBackgroundActions()
	if *busy != 3 {
		t.Fatalf("startup waited for the first interval: %d", *busy)
	}
}

type refillExecBinary struct {
	exec func() error
}

func (refillExecBinary) Replaced() bool         { return true }
func (b refillExecBinary) Exec(...string) error { return b.exec() }

func TestReexecDrainsRefillAndRestartsAfterFailure(t *testing.T) {
	r, busy := fillRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	l.StopRefill = func() { stopped = true }
	filled := make(chan struct{})
	original := r.answer
	r.answer = func(args []string) (string, int) {
		out, code := original(args)
		if args[0] == agentsName && args[1] == agentStartName && *busy == 3 {
			close(filled)
		}
		return out, code
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	l.Bin = refillExecBinary{exec: func() error {
		if !stopped || l.StopRefill != nil {
			t.Error("exec preceded refill drain")
		}
		return errors.New("exec failed")
	}}
	if replaced, err := l.Reexec(ctx); err != nil || replaced {
		t.Fatalf("reexec: %v, %v", replaced, err)
	}
	if l.StopRefill == nil {
		t.Fatal("failed exec lost refill cadence")
	}
	defer l.CancelBackgroundActions()
	defer l.StopRefill()
	select {
	case <-filled:
	case <-time.After(5 * time.Second):
		t.Fatal("failed exec did not resume worker starts")
	}
}

// A configuration replacement waits until the active refill releases its view.
func TestReloadConfigWaitsForRefill(t *testing.T) {
	r, _ := fillRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.RefillConfigMu.RLock()
	entered, done := make(chan struct{}), make(chan struct{})
	go func() { close(entered); l.ReloadConfig(); close(done) }()
	<-entered
	select {
	case <-done:
		t.Error("reload crossed active refill")
	default:
	}
	l.RefillConfigMu.RUnlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reload did not resume after refill")
	}
}
