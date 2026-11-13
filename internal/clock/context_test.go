package clock_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/clock"
)

// TestWithTimeoutEndsOnTheClock checks that WithTimeout's context ends once its
// time has passed on the clock, with context.WithTimeout's Deadline and Err,
// and that its cancel and its parent end it as they end context.WithTimeout's.
//
// control: none: clock.WithTimeout is new, so the merge base has no function to test
func TestWithTimeoutEndsOnTheClock(t *testing.T) {
	ctx, cancel := clock.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if at, ok := ctx.Deadline(); !ok || time.Until(at) > 20*time.Millisecond {
		t.Fatalf("deadline %v (%v), want within 20ms", at, ok)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the context did not end once its time had passed")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) || !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		t.Fatalf("err %v, cause %v, want context.DeadlineExceeded", ctx.Err(), context.Cause(ctx))
	}

	ctx, cancel = clock.WithTimeout(context.Background(), time.Hour)
	cancel()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("cancelled: err %v, want context.Canceled", ctx.Err())
	}

	parent, stop := context.WithCancel(context.Background())
	ctx, cancel = clock.WithTimeout(parent, time.Hour)
	defer cancel()
	stop()
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("parent cancelled: err %v, want context.Canceled", ctx.Err())
	}

	soon, done := context.WithTimeout(context.Background(), time.Minute)
	defer done()
	ctx, cancel = clock.WithTimeout(soon, time.Hour)
	defer cancel()
	if at, _ := ctx.Deadline(); at.After(time.Now().Add(time.Minute)) {
		t.Fatalf("deadline %v, want the parent's earlier one", at)
	}
}
