package cmd

import (
	"context"
	"io"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestRemoteReviewStopIncludesTimerService(t *testing.T) {
	a, l, _ := stopDesk(t)
	l.installed[timerDispatcherUnit] = unitActive
	_, _, failed := a.stopUnits(t.Context(), nil)
	if len(failed) > 0 {
		t.Fatal(failed)
	}
	if !slices.Contains(l.stopped, timerDispatcherUnit) {
		t.Fatal("stop leaves the independent timer service active")
	}
}

func TestStopAndResumeIndependentTimerService(t *testing.T) {
	a, launcher, _ := stopDesk(t)
	launcher.installed[timerDispatcherUnit] = unitActive
	if err := a.stop(t.Context(), "stop timer commands now"); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil || !slices.Contains(st.Stop.Units, timerDispatcherUnit) {
		t.Fatal("dispatcher was not recorded for resume", err)
	}
	if err := a.resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(launcher.resumed, timerDispatcherUnit) {
		t.Fatal("dispatcher did not resume")
	}
}

func TestRemoteReviewOnceObservesStopDuringActiveAction(t *testing.T) {
	a, _ := stubApp(t)
	a.out = io.Discard
	a.loadedCfg = filepath.Join(a.store.Dir(), "config.yaml")
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Timers = []state.Timer{{ID: 1, Due: time.Now().Add(-time.Minute), Wake: "test-worker"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	old := wakeOwner
	started := make(chan struct{})
	wakeOwner = func(_ *app, ctx context.Context, _ state.Party, _, _, _ string) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.runTimerDispatcher(ctx, true) }()
	defer func() { cancel(); <-done; wakeOwner = old }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("action did not start")
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { st.Stop = &state.Stop{At: time.Now()}; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		done <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Error("foreground --once dispatcher ignores stop while its action is active")
	}
}
