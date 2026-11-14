package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	backgroundTestCaller       = "internal/github.rawFile"
	backgroundTestBranchCaller = "internal/github.defaultBranch"
)

func TestWorkerRESTContextKeepsWorkerGate(t *testing.T) {
	a, _ := budgetApp(t)
	t.Setenv(orphanSessionEnv, "background-worker")
	ctx := a.backgroundRESTContext(t.Context())
	if err := github.AdmitRESTCall(ctx, backgroundTestCaller); err != nil {
		t.Fatal(err)
	}
	var spend backgroundRESTSpend
	if found, err := a.store.ReadFile(backgroundRESTFile, &spend); err != nil || found {
		t.Fatalf("worker consumed background allowance: found=%t, err=%v", found, err)
	}
}

func TestBackgroundRESTAllowanceProtectsWindow(t *testing.T) {
	now := time.Now()
	b := &state.Budget{Limit: 5000, Remaining: 4000, Reset: now.Add(time.Hour), At: now.Add(-10 * time.Minute)}
	var spend backgroundRESTSpend
	for range 3000 {
		if err := spend.admit(now, b, 1000, backgroundTestCaller); err != nil {
			t.Fatal(err)
		}
	}
	if err := spend.admit(now, b, 1000, backgroundTestCaller); err == nil || !strings.Contains(err.Error(), "invocation allowance 3000 exhausted") {
		t.Fatalf("unmeasured calls not bounded: %v", err)
	}
	// The same saved reading cannot refund calls. A fresh measurement can.
	b.At = now.Add(time.Second)
	b.Remaining = 1844
	if err := spend.admit(b.At, b, 200, backgroundTestCaller); err != nil {
		t.Fatalf("fresh budget below half the window did not resume: %v", err)
	}
}

func TestBackgroundRESTFloorAndUnknownWindow(t *testing.T) {
	now := time.Now()
	var spend backgroundRESTSpend
	for range 100 {
		if err := spend.admit(now, nil, 0, backgroundTestCaller); err != nil {
			t.Fatal(err)
		}
	}
	if err := spend.admit(now, nil, 0, backgroundTestCaller); err == nil || !strings.Contains(err.Error(), "unknown reading") {
		t.Fatalf("bootstrap cap not named: %v", err)
	}
	b := &state.Budget{Limit: 5000, Remaining: 200, Reset: now.Add(time.Hour), At: now}
	if err := spend.admit(now, b, 200, backgroundTestCaller); err == nil || !strings.Contains(err.Error(), "at or below floor 200") {
		t.Fatalf("floor not named: %v", err)
	}
	b.Remaining = 201
	if err := spend.admit(now, b, 200, backgroundTestCaller); err != nil {
		t.Fatalf("known headroom retained bootstrap hold: %v", err)
	}
	if err := spend.admit(now, b, 200, backgroundTestCaller); err == nil {
		t.Fatal("reservation crossed floor")
	}
	b.Reset = now
	b.Remaining = 5000
	for range 100 {
		if err := spend.admit(now, b, 200, backgroundTestCaller); err != nil {
			t.Fatalf("expired window did not bootstrap: %v", err)
		}
	}
	if err := spend.admit(now, b, 200, backgroundTestCaller); err == nil || !strings.Contains(err.Error(), "bootstrap invocation cap 100 reached") {
		t.Fatalf("expired window exceeded bootstrap cap: %v", err)
	}
}

func TestBackgroundRESTNewWindowAndIdentityRecover(t *testing.T) {
	for _, tc := range []struct {
		name   string
		legacy bool
		mutate func(*state.Budget)
	}{
		{name: "window-reset", mutate: func(b *state.Budget) { b.Reset = b.Reset.Add(-time.Minute) }},
		{name: "credential-identity", mutate: func(b *state.Budget) { b.Identity = "rest-app" }},
		{name: "credential-login", mutate: func(b *state.Budget) { b.Login = "rest-other" }},
		{name: "legacy-ledger", legacy: true, mutate: func(*state.Budget) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			b := &state.Budget{Identity: "rest-gh", Login: "rest-desk", Limit: 5000, Remaining: 4000, Reset: now.Add(time.Hour), At: now}
			var spend backgroundRESTSpend
			if tc.legacy {
				spend = backgroundRESTSpend{Reset: b.Reset, Calls: 4000, Limit: 2000}
			} else {
				for range 3000 {
					if err := spend.admit(now, b, 1000, backgroundTestCaller); err != nil {
						t.Fatal(err)
					}
				}
			}
			tc.mutate(b)
			if err := spend.admit(now, b, 1000, backgroundTestCaller); err != nil {
				t.Fatalf("%s retained exhausted ledger: %v", tc.name, err)
			}
		})
	}
}

func TestBackgroundRESTAllowanceSurvivesProcessRestart(t *testing.T) {
	a, _ := budgetApp(t)
	b, _ := budgetApp(t)
	var err error
	b.store, err = state.Open(a.store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = &state.Budget{Limit: 5000, Remaining: 4000, Reset: time.Now().Add(time.Hour)}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.admitBackgroundREST(t.Context(), backgroundTestCaller); err != nil {
		t.Fatal(err)
	}
	if err := b.admitBackgroundREST(t.Context(), backgroundTestBranchCaller); err != nil {
		t.Fatal(err)
	}
	var spend backgroundRESTSpend
	if _, err := a.store.ReadFile(backgroundRESTFile, &spend); err != nil || spend.Calls != 2 || len(spend.Callers) != 2 {
		t.Fatalf("shared ledger: %+v, %v", spend, err)
	}
}
