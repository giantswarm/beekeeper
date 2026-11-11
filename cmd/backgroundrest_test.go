package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

const backgroundTestCaller = "internal/github.rawFile"

func TestWorkerRESTContextKeepsWorkerGate(t *testing.T) {
	a, _ := budgetApp(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "worker")
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
	b := &state.Budget{Limit: 5000, Remaining: 4000, Reset: now.Add(time.Hour)}
	var spend backgroundRESTSpend
	for i := range backgroundRESTLimit {
		class := backgroundTestCaller
		if i%2 == 0 {
			class = "internal/github.defaultBranch"
		}
		if err := spend.admit(now, b, 1000, class); err != nil {
			t.Fatal(err)
		}
	}
	if err := spend.admit(now, b, 1000, backgroundTestCaller); err == nil || !strings.Contains(err.Error(), backgroundTestCaller) {
		t.Fatalf("caller not stopped or not named: %v", err)
	}
	if spend.Calls != 2000 || spend.Callers[backgroundTestCaller] != 1000 {
		t.Fatalf("caller shares: %+v", spend)
	}
	// A different identity cannot clear the ledger during its live window.
	b.Reset = b.Reset.Add(time.Minute)
	if err := spend.admit(now, b, 1000, backgroundTestCaller); err == nil {
		t.Fatal("window change refunded calls")
	}
	now = b.Reset
	b.Reset = now.Add(time.Hour)
	if err := spend.admit(now, b, 1000, backgroundTestCaller); err != nil || spend.Calls != 1 {
		t.Fatalf("reset did not resume: %+v, %v", spend, err)
	}
}

func TestBackgroundRESTFloorAndUnknownWindow(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct{ limit, remaining, floor int }{
		{5000, 2500, 1000},
		{5000, 3500, 3500},
	} {
		var spend backgroundRESTSpend
		b := &state.Budget{Limit: tc.limit, Remaining: tc.remaining, Reset: now.Add(time.Hour)}
		if err := spend.admit(now, b, tc.floor, backgroundTestCaller); err == nil || spend.Calls != 0 {
			t.Fatalf("floor not protected: %+v, %v", spend, err)
		}
	}
	var spend backgroundRESTSpend
	for range 100 {
		if err := spend.admit(now, nil, 0, backgroundTestCaller); err != nil {
			t.Fatal(err)
		}
	}
	if err := spend.admit(now, nil, 0, backgroundTestCaller); err == nil {
		t.Fatal("unknown window got full allowance")
	}
	b := &state.Budget{Limit: 500, Remaining: 500, Reset: now.Add(time.Hour)}
	for range 149 {
		if err := spend.admit(now, b, 0, backgroundTestCaller); err != nil {
			t.Fatal(err)
		}
	}
	if err := spend.admit(now, b, 0, backgroundTestCaller); err == nil || spend.Calls != 249 {
		t.Fatalf("small window or bootstrap spend lost: %+v, %v", spend, err)
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
	if err := b.admitBackgroundREST(t.Context(), "internal/github.defaultBranch"); err != nil {
		t.Fatal(err)
	}
	var spend backgroundRESTSpend
	if _, err := a.store.ReadFile(backgroundRESTFile, &spend); err != nil || spend.Calls != 2 || len(spend.Callers) != 2 {
		t.Fatalf("shared ledger: %+v, %v", spend, err)
	}
}
