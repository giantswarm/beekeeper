package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

// holdStateLock keeps the state lock exclusively, as a busy loop, watch or
// sessions serve does, until the test ends.
func holdStateLock(t *testing.T, a *app) {
	t.Helper()
	lock := flock.New(filepath.Join(a.store.Dir(), "state.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Unlock() })
}

// Another process holding the state lock past the budget's save does not
// make the budget unknown to the merge gate or to budget --gate: GitHub
// answered, and only keeping the reading lost to the lock (#3411).
func TestBudgetGatesPassWhileTheStateLockIsBusy(t *testing.T) {
	a, out := budgetApp(t)
	refuse := false
	queries := refusingGitHub(t, &refuse)
	holdStateLock(t, a)
	// The probe waits for the lock to save its reading only as long as its
	// caller's deadline allows; a short one keeps the test fast.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	g := &gateRun{app: a, ctx: ctx}
	start := time.Now()
	b, err := g.gateBudget()
	if err != nil || b.Remaining != 4800 {
		t.Fatalf("merge gate: budget %+v, %v with the state lock busy", b, err)
	}
	if time.Since(start) > 10*time.Second || *queries != 1 {
		t.Fatalf("merge gate read the budget in %s with %d GraphQL queries", time.Since(start), *queries)
	}
	c := a.budgetCmd()
	c.SetArgs([]string{gateFlag})
	c.SetOut(out)
	c.SetErr(out)
	start = time.Now()
	if err := c.Execute(); err != nil {
		t.Fatalf("budget --gate = %v with the state lock busy: %s", err, out)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("budget --gate waited %s for the state lock", time.Since(start))
	}
}

// A saved reading younger than merge.budgetFresh is the merge gate's budget
// without the state lock and without GitHub.
func TestMergeGateReadsTheSavedBudgetWithoutTheStateLock(t *testing.T) {
	a, _ := budgetApp(t)
	refuse := false
	queries := refusingGitHub(t, &refuse)
	now := time.Now()
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = &state.Budget{Identity: "gh", Remaining: 4321, Limit: 5000, Reset: now.Add(time.Hour), At: now}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	holdStateLock(t, a)
	a.now = now
	g := &gateRun{app: a, ctx: t.Context()}
	b, err := g.gateBudget()
	if err != nil || b.Remaining != 4321 || *queries != 0 {
		t.Fatalf("merge gate: budget %+v, %v after %d GraphQL queries, want the saved reading", b, err, *queries)
	}
}

// A budget GitHub does not give stays unknown while the lock is busy: the
// busy lock never hides another failure.
func TestBudgetGatesStillRefuseAnUnknownBudgetWhileTheStateLockIsBusy(t *testing.T) {
	a, out := budgetApp(t)
	refuse := true
	refusingGitHub(t, &refuse)
	holdStateLock(t, a)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	g := &gateRun{app: a, ctx: ctx}
	if b, err := g.gateBudget(); err == nil {
		t.Fatalf("merge gate passed a refused GraphQL budget: %+v", b)
	}
	c := a.budgetCmd()
	c.SetArgs([]string{gateFlag})
	c.SetOut(out)
	c.SetErr(out)
	start := time.Now()
	if err := c.Execute(); err == nil {
		t.Fatal("budget --gate passed a refused GraphQL budget")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("budget --gate waited %s for the state lock", time.Since(start))
	}
}

func TestOnlyLockBusy(t *testing.T) {
	busy := &state.LockBusyError{Waited: 100 * time.Millisecond}
	commitBusy := errors.Join(execution.ErrCoordinatorBusy, context.DeadlineExceeded)
	other := errors.New("GitHub identity gh: 401 Bad credentials")
	for _, c := range []struct {
		err  error
		want bool
	}{
		{busy, true},
		{execution.ErrCoordinatorBusy, true},
		{commitBusy, true},
		{fmt.Errorf("saving the budget: %w", busy), true},
		{fmt.Errorf("saving the budget: %w", commitBusy), true},
		{errors.Join(busy, commitBusy), true},
		{errors.Join(other, busy), false},
		{errors.Join(other, commitBusy), false},
		{other, false},
		{errors.Join(), false},
		{context.DeadlineExceeded, false},
	} {
		if got := onlyLockBusy(c.err); got != c.want {
			t.Errorf("onlyLockBusy(%v) = %v, want %v", c.err, got, c.want)
		}
	}
	if unsavedBudget(nil) != nil || unsavedBudget(busy) != nil || unsavedBudget(commitBusy) != nil || !errors.Is(unsavedBudget(errors.Join(other, busy)), other) {
		t.Error("unsavedBudget kept a lone busy lock or dropped another failure")
	}
}

// A loop may hold the producer lock across network probes and state publication.
// A gate must obtain its own bounded reading without publishing out of order.
func TestBudgetGateFallsBackWhenProbeLockIsBusy(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		refuse, savedLow, expired bool
	}{
		{name: "healthy after reset", savedLow: true, expired: true},
		{name: "GraphQL refusal", refuse: true},
		{name: "known low window", savedLow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, out := budgetApp(t)
			refusedGraphQL := tc.refuse
			queries := refusingGitHub(t, &refusedGraphQL)
			if tc.savedLow {
				reset := time.Now().Add(time.Hour)
				if tc.expired {
					reset = time.Now().Add(-time.Second)
				}
				if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.Budget = &state.Budget{Identities: map[string]state.Budget{
						"gh": {Identity: "gh", GraphQL: &state.GraphQL{Identity: "gh", Limit: 5000, Remaining: 10, Reset: reset, At: time.Now()}},
					}}
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			lock := flock.New(filepath.Join(a.store.Dir(), "budget-probe.lock"))
			if err := lock.Lock(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lock.Unlock() }()
			holdStateLock(t, a)
			c := a.budgetCmd()
			c.SetArgs([]string{gateFlag})
			start := time.Now()
			err := c.Execute()
			wantRefused := tc.refuse || tc.savedLow && !tc.expired
			if (err != nil) != wantRefused {
				t.Fatalf("refused=%v: %v; %s", wantRefused, err, out)
			}
			if time.Since(start) > 3*time.Second || *queries != 1 {
				t.Fatalf("fallback took %s, queries %d", time.Since(start), *queries)
			}
			if !strings.Contains(out.String(), "fallback: direct GitHub REST and GraphQL") {
				t.Fatalf("missing fallback warning: %s", out)
			}
		})
	}
}
