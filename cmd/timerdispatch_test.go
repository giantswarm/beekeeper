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

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	timerDispatchRepairWorker = "repair worker"
	timerProofFinished        = "proof finished"
	timerDueEarlier           = "due earlier"
)

// A supervisor inventory can hold admission longer than the fatal timer
// deadline. Timers must defer that check and still deliver unrelated wakes.
func TestTimerDispatcherBoundsGitHubAdmissionWait(t *testing.T) {
	a, _ := stubApp(t)
	a.out = io.Discard
	a.cfg.GitHub.Floor = 100
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = &state.Budget{Remaining: 4900, Reset: a.now.Add(time.Hour),
			GraphQL: &state.GraphQL{Remaining: 4900, Reset: a.now.Add(time.Hour)}}
		st.Timers = []state.Timer{
			{ID: 1, Due: a.now.Add(-time.Minute), When: prMerged7, Wake: workerRole},
			{ID: 2, Due: a.now.Add(-time.Minute), Wake: timerDispatchRepairWorker},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	oldBudget, oldWake := readBudget, wakeOwner
	readBudget = func(*app, context.Context) (github.Budget, error) { return github.Budget{}, nil }
	var wakes atomic.Int32
	wakeOwner = func(_ *app, ctx context.Context, _ state.Party, name, _, _ string) error {
		if name != timerDispatchRepairWorker || ctx.Err() != nil {
			t.Errorf("unrelated wake name=%q context=%v", name, ctx.Err())
		}
		wakes.Add(1)
		return nil
	}
	t.Cleanup(func() { readBudget, wakeOwner = oldBudget, oldWake })
	lock := flock.New(filepath.Join(a.store.Dir(), "loop-graphql.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Unlock() }()
	done := make(chan error, 1)
	go func() {
		w, err := a.dispatchTimerPass(t.Context())
		if err == nil {
			w.TimerActs.Wait()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(fatalProbeTimeout + 5*time.Second):
		_ = lock.Unlock()
		<-done
		t.Fatal("GitHub admission blocked independent timer settlement")
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if wakes.Load() != 1 || len(st.Timers) != 1 || st.Timers[0].ID != 1 || !st.Timers[0].Unreadable || !st.Timers[0].Checked.Equal(a.now) {
		t.Fatalf("wakes=%d timers=%+v", wakes.Load(), st.Timers)
	}
}

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
			w.TimerActs.Wait()
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
		st.Timers = []state.Timer{{ID: 1, Due: a.now.Add(-time.Minute), Wake: timerDispatchRepairWorker, What: timerProofFinished}}
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
			w.TimerActs.Wait()
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
	var batches []*Watcher
	for id := 1; id <= 2; id++ {
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			st.Timers = append(st.Timers, state.Timer{ID: id, Due: a.now.Add(-time.Minute), Wake: timerDispatchRepairWorker, What: timerProofFinished})
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
		w.TimerActs.Wait()
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
	w.TimerActs.Wait()
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
	w.TimerActs.Wait()
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
	w.TimerActs.Wait()
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
		{"plain wake retry on schedule", state.Timer{Due: now.Add(-time.Hour), Checked: now.Add(-time.Minute), PendingWakeReason: timerDueEarlier, Wake: workerRole}, false},
		{"plain wake retry missed", state.Timer{Due: now.Add(-time.Hour), Checked: now.Add(-10 * time.Minute), PendingWakeReason: timerDueEarlier, Wake: workerRole}, true},
		{"conditional retry past expiry", state.Timer{Due: now.Add(-time.Hour), Probe: probeFails, Checked: now.Add(-time.Minute), Until: now.Add(-10 * time.Minute), PendingWakeReason: "timed out", Wake: workerRole}, false},
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
// The pass reads the snapshot without the lock, so the refusal reaches it at
// its settlement.
type timerBusyStore struct{ state.Store }

func (s timerBusyStore) Read() (*state.State, error) {
	return nil, fmt.Errorf("timer read: %w", &state.LockBusyError{})
}

func (s timerBusyStore) Update(func(*state.State) ([]state.Event, error)) error {
	return fmt.Errorf("timer settle: %w", &state.LockBusyError{})
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
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Timers = []state.Timer{{ID: 1, Due: a.now.Add(-time.Minute), Run: probeHolds}}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
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

// Writers queued on the state lock once held a pass's locked reads for most
// of a minute each, so a check ran on its tick but settled past the fatal
// deadline. A writer holding the lock must not hold back the checks: the
// pass checks from the snapshot, and only its settlement waits for the lock.
func TestTimerDispatcherChecksWhileWriterHoldsStateLock(t *testing.T) {
	a, _ := stubApp(t)
	a.out = io.Discard
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Timers = []state.Timer{{ID: 1, Due: a.now.Add(-time.Minute), Probe: probeFails, Wake: timerDispatchRepairWorker}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	probed := make(chan struct{})
	var once sync.Once
	old := readProbe
	readProbe = func(context.Context, string) ([]byte, error) {
		once.Do(func() { close(probed) })
		return nil, errors.New("exit status 1")
	}
	t.Cleanup(func() { readProbe = old })
	locked := make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- a.store.Exclusive(func() error {
			close(locked)
			select {
			case <-probed:
				return nil
			case <-time.After(10 * time.Second):
				return errors.New("condition not checked while a writer held the state lock")
			}
		})
	}()
	select {
	case <-locked:
	case err := <-held:
		t.Fatal("writer did not take the state lock:", err)
	}
	w, err := a.dispatchTimerPass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	w.TimerActs.Wait()
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Timers) != 1 || !st.Timers[0].Checked.Equal(a.now) {
		t.Fatalf("check not settled once the writer released the lock: %+v", st.Timers)
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

// Rejected plain wakes must wait between delivery attempts just like conditional
// wakes, without losing the original occurrence or its delivery reason.
func TestPlainWakeRetryWaitsForInterval(t *testing.T) {
	now := time.Now().UTC()
	timer := state.Timer{ID: 1, Due: now.Add(-time.Hour), Checked: now, Wake: workerRole, PendingWakeReason: timerDueEarlier, Every: time.Minute}
	st := &state.State{Timers: []state.Timer{timer}}
	_, _, fires, changed := SettleTimers(st, nil, now.Add(30*time.Second))
	if changed || len(fires) != 0 || len(st.Timers) != 1 {
		t.Fatal("plain wake retried before its interval", fires, st.Timers)
	}
	_, _, fires, _ = SettleTimers(st, nil, now.Add(time.Minute))
	if len(fires) != 1 || fires[0].Reason != timer.PendingWakeReason || !fires[0].T.Due.Equal(timer.Due) {
		t.Fatal("retry lost its due occurrence", fires)
	}
}

type timerWriteCountingStore struct {
	state.Store
	updates int
	records int
}

func (s *timerWriteCountingStore) Update(fn func(*state.State) ([]state.Event, error)) error {
	s.updates++
	return s.Store.Update(fn)
}

func (s *timerWriteCountingStore) Record(events ...state.Event) error {
	s.records++
	return s.Store.Record(events...)
}

func TestIdleTimerDispatchDoesNotWriteState(t *testing.T) {
	a, _ := stubApp(t)
	store := &timerWriteCountingStore{Store: a.store}
	a.store = store
	w, err := a.dispatchTimerPass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	w.TimerActs.Wait()
	if store.updates != 0 || store.records != 0 {
		t.Fatalf("idle timer dispatch wrote state updates=%d records=%d", store.updates, store.records)
	}
}

func TestTimerDispatchBatchesSkippedAndFailedWakeRetries(t *testing.T) {
	a, _ := stubApp(t)
	a.out = io.Discard
	a.cfg.Guide.Person = personTimo
	oldWake := wakeOwner
	wakeOwner = func(*app, context.Context, state.Party, string, string, string) error {
		return errors.New("target off roster")
	}
	t.Cleanup(func() { wakeOwner = oldWake })
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{
			Party: state.Party{Name: agentOne},
			Park:  &state.Park{On: "#3", Waits: verbAnswer, At: a.now},
		}}
		for id := 1; id <= 10; id++ {
			target := agentOne
			if id > 5 {
				target = timerDispatchRepairWorker
			}
			st.Timers = append(st.Timers, state.Timer{
				ID:    id,
				Due:   a.now.Add(-time.Minute),
				Every: time.Minute,
				Wake:  target,
				What:  timerProofFinished,
			})
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	store := &timerWriteCountingStore{Store: a.store}
	a.store = store
	w, err := a.dispatchTimerPass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	w.TimerActs.Wait()
	if store.updates > 3 || store.records != 0 {
		t.Fatalf("unbounded timer state writes: updates=%d records=%d", store.updates, store.records)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Timers) != 10 {
		t.Fatalf("expected 10 re-armed timers, got %d: %+v", len(st.Timers), st.Timers)
	}
	if got := fatalTimers(st, a.now); got.overdue != 0 {
		t.Fatalf("re-armed batch reported overdue: %+v", got)
	}
}

func TestTimerDispatchSkipsConditionProbesForPendingWakeRetries(t *testing.T) {
	a, _ := stubApp(t)
	a.out = io.Discard
	a.cfg.GitHub.Floor = 100
	checked := a.now.Add(-2 * time.Minute)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Timers = []state.Timer{
			{
				ID:                1,
				Due:               a.now.Add(-10 * time.Minute),
				Checked:           checked,
				Every:             time.Minute,
				When:              prMerged7,
				PendingWakeReason: timerDueEarlier,
				Wake:              timerDispatchRepairWorker,
			},
			{
				ID:                2,
				Due:               a.now.Add(-10 * time.Minute),
				Checked:           checked,
				Every:             time.Minute,
				Probe:             probeHolds,
				PendingWakeReason: timerDueEarlier,
				Wake:              timerDispatchRepairWorker,
			},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	oldBudget, oldReadProbe, oldProbe, oldWake := readBudget, readProbe, RunProbe, wakeOwner
	readBudget = func(*app, context.Context) (github.Budget, error) {
		t.Error("budget sampled for latched pending wake retry")
		return github.Budget{}, errors.New("unexpected budget read")
	}
	readProbe = func(context.Context, string) ([]byte, error) {
		t.Error("check probe executed for latched pending wake retry")
		return nil, errors.New("unexpected probe")
	}
	RunProbe = func(context.Context, string) bool {
		t.Error("shell probe executed for latched pending wake retry")
		return false
	}
	var wakes atomic.Int32
	wakeOwner = func(*app, context.Context, state.Party, string, string, string) error {
		wakes.Add(1)
		return nil
	}
	t.Cleanup(func() { readBudget, readProbe, RunProbe, wakeOwner = oldBudget, oldReadProbe, oldProbe, oldWake })
	lock := flock.New(filepath.Join(a.store.Dir(), "loop-graphql.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Unlock() }()
	w, err := a.dispatchTimerPass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	w.TimerActs.Wait()
	if wakes.Load() != 2 {
		t.Fatalf("expected 2 wakes delivered without re-probing, got %d", wakes.Load())
	}
}

func TestIdleWatchHealthPassesDoNotWriteState(t *testing.T) {
	a, _ := stubApp(t)
	a.out = io.Discard
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.GuideReportsSince = a.now
		st.Guide = &state.Role{
			Holder:    &state.Supervisor{Party: state.Party{Session: "g1", Name: guideName}, Since: a.now},
			WatchSeen: a.now,
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.WriteFile(throughputObservationFile, throughputObservation{At: a.now}); err != nil {
		t.Fatal(err)
	}
	store := &timerWriteCountingStore{Store: a.store}
	a.store = store
	if line, err := a.checkDeliveries(a.now); err != nil || line != "" {
		t.Fatalf("checkDeliveries: %q, %v", line, err)
	}
	if line, err := a.checkThroughput(a.now, false); err != nil || line != "" {
		t.Fatalf("checkThroughput initial: %q, %v", line, err)
	}
	if line, err := a.checkThroughput(a.now.Add(10*time.Second), false); err != nil || line != "" {
		t.Fatalf("checkThroughput within cadence: %q, %v", line, err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	w := a.newWatcher(false, true)
	watchApp(w.Host).now = a.now
	w.GuideMonitor(t.Context(), st, nil)
	if store.updates != 0 || store.records != 0 {
		t.Fatalf("idle watch health passes wrote state updates=%d records=%d", store.updates, store.records)
	}
}
