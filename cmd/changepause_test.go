package cmd

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// A state change wakes the next pass, but not before passChangeGap after the
// previous one: writers that change the state all the time must not keep the
// loop passing back to back.
func TestWaitNextPassChangeKeepsGap(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	r.a.cfg.Supervisor.LoopInterval.Duration = time.Hour
	old := PassChangeGap
	t.Cleanup(func() { PassChangeGap = old })
	PassChangeGap = 600 * time.Millisecond

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	writing := make(chan struct{})
	defer func() { cancel(); <-writing }()
	go func() {
		defer close(writing)
		// Keep changing the state: a write before waitNextPass subscribes
		// must not leave the test waiting for the interval.
		for i := 0; ctx.Err() == nil; i++ {
			_ = r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Notes = []state.Note{{ID: 1, Text: fmt.Sprint(i)}}
				return nil, nil
			})
			time.Sleep(20 * time.Millisecond)
		}
	}()
	start := time.Now()
	if !l.WaitNextPass(ctx) {
		t.Fatal("change did not wake the pass")
	}
	if took := time.Since(start); took < PassChangeGap {
		t.Fatalf("change woke the pass after %s, before the %s gap", took, PassChangeGap)
	}
}
