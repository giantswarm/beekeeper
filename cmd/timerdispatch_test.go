package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

const timerDispatchRepairWorker = "repair worker"

func TestTimerDispatcherStopCancelsEarlierCommandAndResumeRetries(t *testing.T) {
	for _, repeat := range []string{"", state.RepeatDaily} {
		t.Run("repeat="+repeat, func(t *testing.T) {
			a, _ := stubApp(t)
			a.out = io.Discard
			ready := filepath.Join(t.TempDir(), "ready")
			// The shell waits on a child: cancellation must kill both.
			tm := state.Timer{ID: 42, Due: a.now.Add(-time.Minute), Repeat: repeat,
				Run: fmt.Sprintf("touch %s; sleep 60 & wait", ready)}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Timers = []state.Timer{tm}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			d := newTimerDispatchSession(t.Context())
			defer d.close()
			if _, err := d.pass(a); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("command did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Stop = &state.Stop{At: a.now}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			stopped := make(chan error, 1)
			go func() { _, err := d.pass(a); stopped <- err }()
			select {
			case err := <-stopped:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("stop did not cancel and drain active command")
			}
			st, err := a.store.Read()
			if err != nil || len(st.Timers) != 1 || !st.Timers[0].Due.Equal(tm.Due) || !st.Timers[0].Fired.IsZero() {
				t.Fatalf("interrupted occurrence lost: %+v, %v", st, err)
			}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Stop = nil
				st.Timers[0].Run = probeHolds
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			w, err := d.pass(a)
			if err != nil {
				t.Fatal(err)
			}
			w.timerActs.Wait()
			st, err = a.store.Read()
			if err != nil || len(st.Timers) > 0 && !st.Timers[0].Due.After(tm.Due) {
				t.Fatalf("resume did not retry occurrence: %+v, %v", st, err)
			}
		})
	}
}

func TestTimerDispatcherWorksWithoutWatchOrLoopAndClaimsOnce(t *testing.T) {
	a, _ := stubApp(t)
	a.out = io.Discard
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Timers = []state.Timer{{ID: 1, Due: a.now.Add(-time.Minute), Wake: timerDispatchRepairWorker, What: "proof finished"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	old := wakeOwner
	var wakes atomic.Int32
	wakeOwner = func(*app, context.Context, state.Party, string, string, string) error { wakes.Add(1); return nil }
	t.Cleanup(func() { wakeOwner = old })
	var passes sync.WaitGroup
	for range 2 {
		passes.Go(func() {
			w, err := a.dispatchTimerPass(t.Context())
			if err != nil {
				t.Error(err)
				return
			}
			w.timerActs.Wait()
		})
	}
	passes.Wait()
	if wakes.Load() != 1 {
		t.Fatalf("%d wakes", wakes.Load())
	}
}

func TestTimerDispatcherStopRetainsInterruptedWake(t *testing.T) {
	a, _ := stubApp(t)
	a.out = io.Discard
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Timers = []state.Timer{{ID: 1, Due: a.now, Wake: timerDispatchRepairWorker}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	old := wakeOwner
	wakeOwner = func(_ *app, ctx context.Context, _ state.Party, _, _, _ string) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	t.Cleanup(func() { wakeOwner = old })
	d := newTimerDispatchSession(t.Context())
	defer d.close()
	if _, err := d.pass(a); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("wake did not start")
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Stop = &state.Stop{At: a.now}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pass(a); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil || len(st.Timers) != 1 || st.Timers[0].Wake != timerDispatchRepairWorker || !st.Timers[0].Fired.IsZero() {
		t.Fatalf("interrupted wake lost: %+v %v", st, err)
	}
}

func TestTimerDispatcherKeepsTickingDuringEarlierAction(t *testing.T) {
	a, _ := stubApp(t)
	a.out = io.Discard
	old := wakeOwner
	started := make(chan string, 2)
	release := make(chan struct{})
	wakeOwner = func(_ *app, _ context.Context, _ state.Party, name, _, _ string) error {
		started <- name
		<-release
		return nil
	}
	t.Cleanup(func() { wakeOwner = old })
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var batches []*watcher
	for id := 1; id <= 2; id++ {
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			st.Timers = append(st.Timers, state.Timer{ID: id, Due: a.now.Add(-time.Minute), Wake: timerDispatchRepairWorker, What: "proof finished"})
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		w, err := a.dispatchTimerPass(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		batches = append(batches, w)
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("earlier action blocked a later timer")
		}
	}
	// Release and wait before restoring the global wake implementation.
	unblock()
	for _, w := range batches {
		w.timerActs.Wait()
	}
}

func TestTimerDispatcherHonorsStopForCommandsAndWakes(t *testing.T) {
	a, _ := stubApp(t)
	a.out = io.Discard
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Stop = &state.Stop{At: a.now}
		st.Timers = []state.Timer{{ID: 1, Due: a.now.Add(-time.Minute), Run: probeFails}, {ID: 2, Due: a.now.Add(-time.Minute), Wake: workerRole}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	w, err := a.dispatchTimerPass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	w.timerActs.Wait()
	st, err := a.store.Read()
	if err != nil || len(st.Timers) != 2 {
		t.Fatal("stop did not retain timers", err)
	}
}

func TestTimerDispatcherSamplesItsOwnGitHubBudget(t *testing.T) {
	a, _ := stubApp(t)
	a.out = io.Discard
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = nil
		st.Timers = []state.Timer{{ID: 1, Due: a.now.Add(-time.Hour), When: prMerged7, Wake: workerRole}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	old := readBudget
	reads := 0
	readBudget = func(*app, context.Context) (github.Budget, error) {
		reads++
		return github.Budget{}, errors.New("budget unavailable")
	}
	t.Cleanup(func() { readBudget = old })
	w, err := a.dispatchTimerPass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	w.timerActs.Wait()
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 || len(st.Timers) != 1 || !st.Timers[0].Checked.Equal(a.now) || !st.Timers[0].Unreadable {
		t.Fatalf("budget reads %d, timers %+v", reads, st.Timers)
	}
}

func TestTimerDispatcherPreservesLongCheckIntervals(t *testing.T) {
	a, _ := stubApp(t)
	a.out = io.Discard
	checked := a.now.Add(-10 * time.Minute)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Timers = []state.Timer{{ID: 1, Due: a.now.Add(-time.Hour), When: prMerged7, Checked: checked, Every: time.Hour, Wake: workerRole}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	old := readBudget
	readBudget = func(*app, context.Context) (github.Budget, error) {
		t.Error("budget checked before timer interval")
		return github.Budget{}, errors.New("unavailable")
	}
	t.Cleanup(func() { readBudget = old })
	w, err := a.dispatchTimerPass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	w.timerActs.Wait()
	st, err := a.store.Read()
	if err != nil || !st.Timers[0].Checked.Equal(checked) {
		t.Fatal("deferred timer interval was restarted", err)
	}
}

func TestFatalTimersDetectMissedTicksNotUnsatisfiedConditions(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name    string
		timer   state.Timer
		overdue bool
	}{
		{"wake overdue", state.Timer{Due: now.Add(-time.Hour), Wake: workerRole}, true},
		{"not due yet", state.Timer{Due: now.Add(time.Minute), Wake: workerRole}, false},
		{"grace", state.Timer{Due: now.Add(-time.Minute), Wake: workerRole}, false},
		{"never checked", state.Timer{Due: now.Add(-time.Hour), Probe: probeFails, Wake: workerRole}, true},
		{"recently checked false", state.Timer{Due: now.Add(-time.Hour), Probe: probeFails, Checked: now.Add(-time.Minute), Wake: workerRole}, false},
		{"missed check", state.Timer{Due: now.Add(-time.Hour), Probe: probeFails, Checked: now.Add(-10 * time.Minute), Wake: workerRole}, true},
		{"custom interval", state.Timer{Due: now.Add(-time.Hour), Probe: probeFails, Checked: now.Add(-10 * time.Minute), Every: time.Hour, Wake: workerRole}, false},
		{"missed expiry", state.Timer{Due: now.Add(-time.Hour), Probe: probeFails, Checked: now, Until: now.Add(-10 * time.Minute), Wake: workerRole}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &state.State{Timers: []state.Timer{tc.timer}}
			got := fatalTimers(st, now)
			if (got.overdue > 0) != tc.overdue {
				t.Fatalf("%+v", got)
			}
			st.Stop = &state.Stop{}
			if !fatalTimers(st, now).disabled {
				t.Fatal("intentional stop reported as outage")
			}
		})
	}
}

// A temporary lock refusal must not terminate the long-running scheduler.
// --once still reports it to its caller instead of claiming a successful pass.
type timerBusyStore struct{ state.Store }

func (s timerBusyStore) Read() (*state.State, error) {
	return nil, fmt.Errorf("timer read: %w", &state.LockBusyError{})
}

type timerCancelWriter struct {
	cancel context.CancelFunc
	wrote  bool
}

func (w *timerCancelWriter) Write(p []byte) (int, error) {
	w.wrote = true
	w.cancel()
	return len(p), nil
}
func TestTimerDispatcherBusyLockKeepsServiceAlive(t *testing.T) {
	for _, once := range []bool{false, true} {
		t.Run(fmt.Sprint(once), func(t *testing.T) {
			a, _ := stubApp(t)
			a.loadedCfg = filepath.Join(a.store.Dir(), "config.yaml")
			a.store = timerBusyStore{a.store}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			out := &timerCancelWriter{cancel: cancel}
			a.out = out
			err := a.runTimerDispatcher(ctx, once)
			var busy *state.LockBusyError
			if once {
				if !errors.As(err, &busy) || out.wrote {
					t.Fatalf("once: err=%v logged=%v", err, out.wrote)
				}
			} else if err != nil || !out.wrote {
				t.Fatalf("service exited instead of retrying: err=%v logged=%v", err, out.wrote)
			}
		})
	}
}

// Simulate a writer that prevents locked reads. Stop is already committed
// in the atomic snapshot and must cancel dispatch without waiting for it.
type timerStopSnapshotStore struct {
	state.Store
	release chan struct{}
	once    sync.Once
}

func (s *timerStopSnapshotStore) Read() (*state.State, error) {
	<-s.release
	return nil, &state.LockBusyError{}
}
func (s *timerStopSnapshotStore) Peek() (*state.State, error) {
	s.once.Do(func() { close(s.release) })
	return &state.State{Stop: &state.Stop{At: time.Now()}}, nil
}
func TestTimerDispatcherObservesStopWithoutStateLock(t *testing.T) {
	a, _ := stubApp(t)
	a.loadedCfg = filepath.Join(a.store.Dir(), "config.yaml")
	store := &timerStopSnapshotStore{Store: a.store, release: make(chan struct{})}
	a.store = store
	a.out = io.Discard
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.runTimerDispatcher(ctx, false) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		cancel()
		store.once.Do(func() { close(store.release) })
		<-done
		t.Fatal("stop monitor waited for the state lock")
	}
}
