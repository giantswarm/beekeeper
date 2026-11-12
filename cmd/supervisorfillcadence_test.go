package cmd

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

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
	l.independentFill = true
	r.park(t, loopPR, r.a.timeNow())
	entered, release := make(chan struct{}), make(chan struct{})
	original := readLoopPull
	var first sync.Once
	readLoopPull = func(ctx context.Context, ref string) (superloop.Pull, error) {
		first.Do(func() { close(entered); <-release })
		return original(ctx, ref)
	}
	passDone := make(chan error, 1)
	go func() { passDone <- l.pass(t.Context()) }()
	defer func() {
		close(release)
		if err := <-passDone; err != nil {
			t.Error(err)
		}
		l.passActions.Wait()
		l.mergeActions.Wait()
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
	go func() { defer close(done); l.runRefillCadence(ctx, ticks) }()
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

func TestRefillCadenceHonoursHoldAndRecovers(t *testing.T) {
	r, busy := fillRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.holdStarts(true)
	if err := l.refillTick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *busy != 0 {
		t.Fatal("started through a hold")
	}
	l.holdStarts(false)
	if err := l.refillTick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *busy != 3 {
		t.Fatalf("did not recover: %d workers", *busy)
	}
}

func TestRefillStallNamesActiveAction(t *testing.T) {
	r, _ := fillRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.fillRan()
	l.setFillPhase("board next")
	r.a.now = r.a.now.Add(loopStallAfter)
	if err := l.checkStall(); err != nil {
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
	l.backgroundContext(t.Context())
	defer l.cancelBackgroundActions()
	original := beekeeperRun
	entered := make(chan struct{})
	beekeeperRun = func(ctx context.Context, args ...string) (string, int, error) {
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
	go func() { defer close(done); l.runRefillCadence(ctx, ticks) }()
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
	if l.reexecBusy() {
		t.Fatal("cancelled refill retained its in-flight reservation")
	}
}

func TestRefillCadenceDoesNotOverlap(t *testing.T) {
	r, busy := fillRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.beginInFlight(loopFillStream)
	defer l.endInFlight(loopFillStream)
	if err := l.refillTick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *busy != 0 {
		t.Fatal("started a second refill while the first was active")
	}
}
