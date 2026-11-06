package cmd

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestTimerDispatcherWorksWithoutWatchOrLoopAndClaimsOnce(t *testing.T) {
	a, _ := stubApp(t)
	a.out = io.Discard
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Timers = []state.Timer{{ID: 1, Due: a.now.Add(-time.Minute), Wake: "repair worker", What: "proof finished"}}
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
			st.Timers = append(st.Timers, state.Timer{ID: id, Due: a.now.Add(-time.Minute), Wake: "repair worker", What: "proof finished"})
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
		st.Timers = []state.Timer{{ID: 1, Due: a.now.Add(-time.Minute), Run: probeFails}, {ID: 2, Due: a.now.Add(-time.Minute), Wake: "worker"}}
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
		st.Timers = []state.Timer{{ID: 1, Due: a.now.Add(-time.Hour), When: prMerged7, Wake: "worker"}}
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
		st.Timers = []state.Timer{{ID: 1, Due: a.now.Add(-time.Hour), When: prMerged7, Checked: checked, Every: time.Hour, Wake: "worker"}}
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
		{"wake overdue", state.Timer{Due: now.Add(-time.Hour), Wake: "worker"}, true},
		{"not due yet", state.Timer{Due: now.Add(time.Minute), Wake: "worker"}, false},
		{"grace", state.Timer{Due: now.Add(-time.Minute), Wake: "worker"}, false},
		{"never checked", state.Timer{Due: now.Add(-time.Hour), Probe: probeFails, Wake: "worker"}, true},
		{"recently checked false", state.Timer{Due: now.Add(-time.Hour), Probe: probeFails, Checked: now.Add(-time.Minute), Wake: "worker"}, false},
		{"missed check", state.Timer{Due: now.Add(-time.Hour), Probe: probeFails, Checked: now.Add(-10 * time.Minute), Wake: "worker"}, true},
		{"custom interval", state.Timer{Due: now.Add(-time.Hour), Probe: probeFails, Checked: now.Add(-10 * time.Minute), Every: time.Hour, Wake: "worker"}, false},
		{"missed expiry", state.Timer{Due: now.Add(-time.Hour), Probe: probeFails, Checked: now, Until: now.Add(-10 * time.Minute), Wake: "worker"}, true},
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
