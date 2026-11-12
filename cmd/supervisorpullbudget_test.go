package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

// pullBudgetRig is a pull watcher tracking loopPR with github.floor 1000,
// its listing done, the GitHub budget readings answered by read through the
// real refreshLoopBudget, and the batch reads counted.
func pullBudgetRig(t *testing.T, read func() (github.Budget, error)) (*loopRig, *loop.Loop, *int, *int) {
	t.Helper()
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = 1000
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	w := l.PullWatcher()
	w.Set([]string{loopPR}, nil, nil, 1000, nil)
	w.Snapshot.ListedAt = time.Now()
	oldRefresh, oldRead, oldBatch := refreshLoopBudget, readLoopGraphQLBudget, loop.ReadLoopPullBatch
	t.Cleanup(func() {
		refreshLoopBudget, readLoopGraphQLBudget, loop.ReadLoopPullBatch = oldRefresh, oldRead, oldBatch
	})
	refreshLoopBudget = readFreshLoopBudget
	reads, batches := new(int), new(int)
	readLoopGraphQLBudget = func(*app, context.Context) (github.Budget, error) {
		*reads++
		return read()
	}
	loop.ReadLoopPullBatch = func(context.Context, []string, map[string]loop.WatchedLoopPull, int) (map[string]loop.WatchedLoopPull, error) {
		*batches++
		return map[string]loop.WatchedLoopPull{loopPR: {Pull: r.pull, At: time.Now()}}, nil
	}
	return r, l, reads, batches
}

func saveBudget(t *testing.T, r *loopRig, b *state.Budget) {
	t.Helper()
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = b
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func ampleReading(remaining int, reset time.Duration) github.Budget {
	now := time.Now()
	return github.Budget{Remaining: 4800, Limit: 5000, Reset: now.Add(time.Hour),
		GraphQL: &github.GraphQL{Remaining: remaining, Limit: 5000, Reset: now.Add(reset)}}
}

// The budget is ample, but the saved reading is twenty minutes old: no
// watch sample or pass has saved a newer one. The pull watch takes its own
// reading, as a held pass does (#3724), and reads its pull requests,
// rather than holding on "budget unknown" and backing off for up to ten
// minutes while every observation goes stale (#3841). A reading GitHub
// answered stands when only saving it failed on a busy state or lease
// commit. A fresh reading under the floor and the batch's reserve holds
// the watch until the window resets.
func TestPullWatcherReadsTheBudgetItselfOnAStaleReading(t *testing.T) {
	busyLease := errors.Join(execution.ErrCoordinatorBusy, context.DeadlineExceeded)
	for _, tc := range []struct {
		name      string
		remaining int
		saveErr   error
		held      string
	}{
		{name: "ample", remaining: 4076},
		{name: "ample, its save lost to a busy state lock", remaining: 4076, saveErr: &state.LockBusyError{Waited: 2 * time.Minute}},
		{name: "ample, its save lost to a busy lease commit", remaining: 4076, saveErr: busyLease},
		{name: "low", remaining: 1200, held: "GitHub GraphQL budget 1200 of 5000 under github.floor 1000 and the 500 kept above it until"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, l, reads, batches := pullBudgetRig(t, func() (github.Budget, error) {
				return ampleReading(tc.remaining, 50*time.Minute), tc.saveErr
			})
			stale := time.Now().Add(-20 * time.Minute)
			saveBudget(t, r, &state.Budget{Remaining: 4800, Limit: 5000, Reset: time.Now().Add(time.Hour), At: stale,
				GraphQL: &state.GraphQL{Remaining: 4500, Limit: 5000, Reset: time.Now().Add(40 * time.Minute), At: stale}})
			var schedule loop.LoopPullSchedule
			next, err := l.PullWatcher().Poll(t.Context(), l)
			retry := schedule.After(l, next, err)
			if *reads != 1 {
				t.Fatalf("the watch read the budget %d times, want 1", *reads)
			}
			if tc.held == "" {
				if err != nil || *batches != 1 {
					t.Fatalf("an ample fresh reading held the watch: %d batches, %v\n%s", *batches, err, r.out)
				}
				if _, err := l.PullWatcher().Read(loopPR, time.Now()); err != nil {
					t.Fatal(err)
				}
				return
			}
			var hold *loop.PullBudgetHold
			if !errors.As(err, &hold) || !errors.Is(err, loop.ErrLoopLater) || *batches != 0 {
				t.Fatalf("a fresh low reading did not hold: %d batches, %v", *batches, err)
			}
			if !strings.Contains(err.Error(), tc.held) {
				t.Fatalf("the hold %q does not say %q", err, tc.held)
			}
			// Nothing ends it before the window resets: the watch tries
			// again at the longest backoff, not sooner.
			if retry != 10*time.Minute {
				t.Fatalf("retry %s, want 10m0s", retry)
			}
		})
	}
}

// The watch's own reading has its own bound, apart from the minute of the
// listing and the batch: on a busy state commit it waits out most of its 30
// seconds, and a batch left only the rest of a shared minute is killed
// before GitHub answers it (#3841).
func TestPullWatcherBatchKeepsItsMinuteAfterItsBudgetReading(t *testing.T) {
	var read time.Time
	r, l, reads, batches := pullBudgetRig(t, func() (github.Budget, error) {
		read = time.Now()
		return ampleReading(4076, 50*time.Minute), &state.LockBusyError{Waited: 30 * time.Second}
	})
	loop.ReadLoopPullBatch = func(ctx context.Context, _ []string, _ map[string]loop.WatchedLoopPull, _ int) (map[string]loop.WatchedLoopPull, error) {
		*batches++
		deadline, ok := ctx.Deadline()
		if !ok || deadline.Sub(read) < time.Minute {
			t.Errorf("the batch's deadline is %s after the budget reading (set %v), want its whole minute", deadline.Sub(read), ok)
		}
		return map[string]loop.WatchedLoopPull{loopPR: {Pull: r.pull, At: time.Now()}}, nil
	}
	if _, err := l.PullWatcher().Poll(t.Context(), l); err != nil || *reads != 1 || *batches != 1 {
		t.Fatalf("an ample reading held the watch: %d readings, %d batches, %v", *reads, *batches, err)
	}
}

// A hold names its cause, and a reading the watch could not take is tried
// again within two minutes: on a loaded desk the watch held 30 minutes and more
// on an unknown reading with GitHub's budget ample (#3841).
func TestPullWatcherBudgetHoldNamesItsCause(t *testing.T) {
	t.Run("unknown", func(t *testing.T) {
		r, l, reads, batches := pullBudgetRig(t, func() (github.Budget, error) {
			return github.Budget{}, errors.New("GitHub identity gh: gh credential unavailable: context deadline exceeded")
		})
		var schedule loop.LoopPullSchedule
		var retries []time.Duration
		for range 4 {
			next, err := l.PullWatcher().Poll(t.Context(), l)
			if !errors.Is(err, loop.ErrLoopLater) {
				t.Fatalf("an unknown budget did not hold: %v", err)
			}
			retries = append(retries, schedule.After(l, next, err))
		}
		if *reads != 4 || *batches != 0 {
			t.Fatalf("%d readings, %d batches; want a reading on each try and no batch", *reads, *batches)
		}
		if got, want := fmt.Sprint(retries), fmt.Sprint([]time.Duration{time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute}); got != want {
			t.Fatalf("retries %s, want %s", got, want)
		}
		for _, want := range []string{"SUPERVISOR PULL WATCH held: GitHub budget unknown: no reading", "own reading failed", "context deadline exceeded", "next try in 1m0s"} {
			if !strings.Contains(r.out.String(), want) {
				t.Fatalf("the held line does not say %q:\n%s", want, r.out)
			}
		}
		if n := strings.Count(r.out.String(), "SUPERVISOR PULL WATCH held"); n != 1 {
			t.Fatalf("the hold was said %d times, want once", n)
		}
	})
	t.Run("low until a near reset", func(t *testing.T) {
		r, l, reads, batches := pullBudgetRig(t, func() (github.Budget, error) {
			t.Fatal("a fresh reading was read again")
			return github.Budget{}, nil
		})
		now := time.Now()
		saveBudget(t, r, &state.Budget{Remaining: 4800, Limit: 5000, Reset: now.Add(time.Hour), At: now,
			GraphQL: &state.GraphQL{Remaining: 900, Limit: 5000, Reset: now.Add(3 * time.Minute), At: now}})
		var schedule loop.LoopPullSchedule
		next, err := l.PullWatcher().Poll(t.Context(), l)
		retry := schedule.After(l, next, err)
		if *reads != 0 || *batches != 0 || !errors.Is(err, loop.ErrLoopLater) {
			t.Fatalf("%d readings, %d batches, %v; want a hold on the saved reading", *reads, *batches, err)
		}
		if !strings.Contains(err.Error(), "GitHub GraphQL budget 900 of 5000 under github.floor 1000 until") {
			t.Fatalf("the hold does not name the low budget: %v", err)
		}
		// The window resets in three minutes: the watch tries again then.
		if retry > 3*time.Minute || retry < 2*time.Minute {
			t.Fatalf("retry %s, want the reset three minutes away", retry)
		}
	})
	t.Run("a reset window", func(t *testing.T) {
		r, l, reads, batches := pullBudgetRig(t, func() (github.Budget, error) {
			return ampleReading(5000, time.Hour), nil
		})
		then := time.Now().Add(-50 * time.Second)
		saveBudget(t, r, &state.Budget{Remaining: 4800, Limit: 5000, Reset: then.Add(time.Hour), At: then,
			GraphQL: &state.GraphQL{Remaining: 800, Limit: 5000, Reset: time.Now().Add(-time.Second), At: then}})
		if _, err := l.PullWatcher().Poll(t.Context(), l); err != nil || *reads != 1 || *batches != 1 {
			t.Fatalf("a low reading of a window that has reset held the watch: %d readings, %d batches, %v", *reads, *batches, err)
		}
	})
}

// The loop's GitHub admission takes a reading before and after every step.
// A reading GitHub answered stands when only saving it failed on a busy
// state or lease commit, as for budget --gate (#3411); before, the loop
// held every GitHub step as "budget unavailable" while the commit lock was
// contended (#3841). A reading that failed holds and says why.
func TestLoopGitHubKeepsAReadingLostToABusyCommit(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = 1000
	healthyLoopBudget(t, r)
	old := readLoopGraphQLBudget
	t.Cleanup(func() { readLoopGraphQLBudget = old })
	var readErr error
	readLoopGraphQLBudget = func(*app, context.Context) (github.Budget, error) {
		return github.Budget{Remaining: 4900, Limit: 5000, Reset: r.a.now.Add(time.Hour),
			GraphQL: &github.GraphQL{Remaining: 4000, Limit: 5000, Reset: r.a.now.Add(time.Hour)}}, readErr
	}
	for _, busy := range []error{
		&state.LockBusyError{Waited: 2 * time.Minute},
		errors.Join(errors.Join(execution.ErrCoordinatorBusy, context.DeadlineExceeded)),
	} {
		readErr = busy
		ran := false
		if err := r.a.loopGitHub(t.Context(), loop.LoopPullStep, func() error { ran = true; return nil }); err != nil || !ran {
			t.Fatalf("a reading lost to %v held the step (ran %v): %v", busy, ran, err)
		}
	}
	readErr = errors.New("GitHub identity gh: gh credential unavailable: context deadline exceeded")
	err := r.a.loopGitHub(t.Context(), loop.LoopPullStep, func() error {
		t.Fatal("a failed reading admitted the step")
		return nil
	})
	if !errors.Is(err, loop.ErrLoopLater) || !strings.Contains(err.Error(), "the reading failed: GitHub identity gh: gh credential unavailable: context deadline exceeded") {
		t.Fatalf("a failed reading does not hold naming its failure: %v", err)
	}
}

// The pass's own reading stands too when a busy commit kept it from being
// saved: it reads the board on it rather than holding on the stale saved
// reading (#3841). The held line names the cause.
func TestLoopPassHoldsOnItsOwnReadingWhenItsSaveIsLost(t *testing.T) {
	for _, low := range []bool{false, true} {
		t.Run(fmt.Sprint("low=", low), func(t *testing.T) {
			r, l, boardReads := emptyBoardFill(t)
			r.a.cfg.GitHub.Floor = 1000
			stale := r.a.now.Add(-20 * time.Minute)
			saveBudget(t, r, &state.Budget{Remaining: 4800, Limit: 5000, Reset: r.a.now.Add(time.Hour), At: stale,
				GraphQL: &state.GraphQL{Remaining: 4076, Limit: 5000, Reset: r.a.now.Add(10 * time.Minute), At: stale}})
			oldRefresh, oldRead := refreshLoopBudget, readLoopGraphQLBudget
			t.Cleanup(func() { refreshLoopBudget, readLoopGraphQLBudget = oldRefresh, oldRead })
			refreshLoopBudget = readFreshLoopBudget
			remaining := 4076
			if low {
				remaining = 400
			}
			readLoopGraphQLBudget = func(a *app, _ context.Context) (github.Budget, error) {
				now := a.timeNow()
				return github.Budget{Remaining: 4800, Limit: 5000, Reset: now.Add(time.Hour),
					GraphQL: &github.GraphQL{Remaining: remaining, Limit: 5000, Reset: now.Add(50 * time.Minute)}}, &state.LockBusyError{Waited: time.Minute}
			}
			r.loopPass(t, l)
			if low {
				if *boardReads != 0 || !strings.Contains(r.out.String(), "SUPERVISOR GITHUB HELD: GraphQL budget unknown or under github.floor (GitHub GraphQL budget 400 of 5000 under github.floor 1000 until") {
					t.Fatalf("a fresh low reading did not hold naming its budget: %d board reads\n%s", *boardReads, r.out)
				}
				return
			}
			if *boardReads != 1 || strings.Contains(r.out.String(), "SUPERVISOR GITHUB HELD") {
				t.Fatalf("an ample reading whose save was lost held the pass: %d board reads\n%s", *boardReads, r.out)
			}
		})
	}
}
