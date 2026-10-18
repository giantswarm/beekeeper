//go:build unix

package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

// resetApp is queueApp under the budget floor (40 of 100) until a reset
// in a second, merge.resetFirst 5m, the readings polled every 10ms; after
// the reset the budget reads 5000.
func resetApp(t *testing.T, merges ...state.Merge) (*app, time.Time) {
	t.Helper()
	a := queueApp(t, merges...)
	a.cfg.Merge.BudgetFresh = config.Duration{Duration: time.Hour}
	a.cfg.Merge.ResetFirst = config.Duration{Duration: 5 * time.Minute}
	reset := time.Now().Add(time.Second)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = &state.Budget{Remaining: 40, Limit: 5000, Reset: reset, At: time.Now()}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	read, poll := readBudget, yieldPoll
	t.Cleanup(func() { readBudget, yieldPoll = read, poll })
	yieldPoll = 10 * time.Millisecond
	readBudget = func(*app, context.Context) (github.Budget, error) {
		if time.Now().Before(reset) {
			return github.Budget{Remaining: 40, Limit: 5000, Reset: reset}, nil
		}
		return github.Budget{Remaining: 5000, Limit: 5000, Reset: reset.Add(time.Hour)}, nil
	}
	return a, reset
}

// yields says whether a gated wait started now still yields to the merges
// queued for the reset after d.
func yields(t *testing.T, a *app, d time.Duration) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	err := a.yieldToMerges(ctx, strings.Fields("devctl pr wait o/r 8"))
	return errors.Is(err, context.DeadlineExceeded)
}

// A merge queued under the floor runs at the first reset after it was
// queued, before any wait: the reading of the window that ended is not
// gated on, the waits' devctl processes do not count against merge.cap
// for it, and a wait started at the reset starts after it. The waits run
// with what the merge leaves.
func TestAMergeQueuedUnderTheFloorRunsFirstAtTheReset(t *testing.T) {
	noSystemd(t)
	fakeDevctl(t, `echo '`+mergedDoc+`'`)
	stubGitHub(t, github.Merged, "")
	stubBaseRelease(t, github.BaseRelease{Base: mainBranch, Auto: true}, nil)
	a, reset := resetApp(t)
	g := &gateRun{app: a, ctx: context.Background(), argv: mergeArgv(scratchRepo), repo: scratchRepo, pr: 7, lane: a.cfg.LaneOf(scratchRepo),
		me: state.Party{Session: "s1", Name: ownerName}, pid: os.Getpid(), queued: true}
	if why, err := g.step(); err != nil || !strings.Contains(why, "under the floor 100 until the reset") {
		t.Fatalf("step: %q, %v; want a wait for the reset", why, err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Merges) != 1 || !st.Merges[0].Reset.Equal(reset.UTC()) {
		t.Fatalf("merges %+v, want o/r#7 waiting for the reset at %s", st.Merges, reset)
	}
	if yields(t, a, 50*time.Millisecond) {
		t.Error("a wait yields before the reset came")
	}

	time.Sleep(time.Until(reset) + 10*time.Millisecond)
	a.now = time.Now()
	if !yields(t, a, 50*time.Millisecond) {
		t.Error("a wait started at the reset did not yield to the queued merge")
	}
	if st, err = a.store.Read(); err != nil || !a.resetFirst(st) {
		t.Errorf("the watch's GitHub timer checks do not wait for the queued merge (%v)", err)
	}

	// The waits resumed at the reset hold every devctl slot.
	hostDevctls(t, a.cfg.Merge.Cap)
	if err := a.gate(context.Background(), mergeArgv(scratchRepo), 10*time.Second, 0, true); err != nil {
		t.Fatalf("exit %d (%v), want the merge run at the reset", Code(err), err)
	}
	if d := lastEventOf(t, a, verbMerged); !strings.Contains(d, "o/r#7 exit 0") {
		t.Errorf("merged event %q", d)
	}
	if yields(t, a, 50*time.Millisecond) {
		t.Error("a wait still yields after the merge ran")
	}
}

// At the reset the older merge queued for it takes a devctl slot first;
// the waits' processes count for neither.
func TestTheOldestMergeQueuedForTheResetGoesFirst(t *testing.T) {
	reset := time.Now().Add(-time.Minute)
	older := state.Merge{Repo: "o/s", PR: 3, Lane: "o/s", By: state.Party{Name: aheadName}, PID: sleeper(t), Phase: state.Waiting,
		Joined: time.Now().Add(-time.Hour), Seen: time.Now(), Reset: reset}
	me := state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, By: state.Party{Session: "s1", Name: ownerName}, PID: os.Getpid(), Phase: state.Waiting,
		Joined: time.Now().Add(-30 * time.Minute), Seen: time.Now(), Reset: reset}
	running := state.Merge{Repo: "o/x", PR: 1, Lane: "o/x", By: state.Party{Name: aheadName}, PID: sleeper(t), Phase: state.Running,
		Joined: time.Now().Add(-2 * time.Hour), Started: time.Now()}
	a := queueApp(t, older, me, running)
	a.cfg.Merge.ResetFirst = config.Duration{Duration: 5 * time.Minute}
	a.cfg.Merge.Cap = 2
	hostDevctls(t, 10)
	g := &gateRun{app: a, ctx: context.Background(), repo: scratchRepo, pr: 7, lane: a.cfg.LaneOf(scratchRepo), me: me.By, pid: os.Getpid()}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if why := g.capWait(st, me); !strings.Contains(why, "1 merges run (cap 2) and o/s#3, queued for the reset before it, goes first") {
		t.Errorf("cap wait %q, want o/s#3 first", why)
	}
	if first := merge.ResetFirst(st, a.now, time.Hour, func(int) bool { return true }); merge.Keys(first) != "o/s#3, o/r#7" {
		t.Errorf("reset first %q, want the oldest queued first", merge.Keys(first))
	}
	a.cfg.Merge.Cap = 3
	if why := g.capWait(st, me); why != "" {
		t.Errorf("cap wait %q with a slot for both, want none", why)
	}
	// Past its window the merge counts the machine's merges and promotes again.
	a.now = reset.Add(a.cfg.Merge.ResetFirst.Duration)
	if why := g.capWait(st, me); !strings.Contains(why, "11 merges and promotes run machine-wide (cap 3)") {
		t.Errorf("cap wait %q past the window, want the machine's merges and promotes", why)
	}
}

// budget --gate refuses while the merges queued for the reset go first and
// names them; the waits run once the window ends.
func TestBudgetGateLetsTheMergesQueuedForTheResetGoFirst(t *testing.T) {
	reset := time.Now().Add(-time.Minute)
	a := queueApp(t, state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, By: state.Party{Name: aheadName}, PID: sleeper(t),
		Phase: state.Waiting, Joined: time.Now().Add(-time.Hour), Seen: time.Now(), Reset: reset})
	a.cfg.Merge.ResetFirst = config.Duration{Duration: 5 * time.Minute}
	read := readBudget
	t.Cleanup(func() { readBudget = read })
	readBudget = func(*app, context.Context) (github.Budget, error) {
		return github.Budget{Remaining: 5000, Limit: 5000, Reset: time.Now().Add(time.Hour)}, nil
	}
	gate := func() (string, error) {
		var out bytes.Buffer
		a.out = &out
		c := a.budgetCmd()
		c.SetArgs([]string{"--gate"})
		c.SetOut(&out)
		c.SetErr(&out)
		err := c.Execute()
		return out.String(), err
	}
	out, err := gate()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "merges first: o/r#7, queued for the reset, go before the waits") {
		t.Errorf("budget --gate: exit %d (%v), want refused for o/r#7", Code(err), err)
	}
	if !strings.Contains(out, "merges first: o/r#7") {
		t.Errorf("budget output %q, want the merges-first line", out)
	}
	a.now = reset.Add(a.cfg.Merge.ResetFirst.Duration)
	if _, err := gate(); err != nil {
		t.Errorf("budget --gate past the window: %v, want the waits let through", err)
	}
}
