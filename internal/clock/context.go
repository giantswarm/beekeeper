package clock

import (
	"context"
	"errors"
	"time"
)

// WithTimeout is context.WithTimeout on the clock: the context ends once d
// has passed on it, the world's time in a scenario. Deadline and Err answer
// as context.WithTimeout's do: Err is context.DeadlineExceeded once d has
// passed.
func WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	at := Now().Add(d)
	ctx, cancel := context.WithCancelCause(parent)
	t := AfterFunc(d, func() { cancel(context.DeadlineExceeded) })
	// A distinct Done channel keeps context's cancellation propagation from
	// bypassing deadline.Err and copying the embedded context's Canceled error
	// into children when our timer expires. It also stops the timer when the
	// parent ends before the deadline.
	done := make(chan struct{})
	context.AfterFunc(ctx, func() {
		t.Stop()
		close(done)
	})
	return deadline{Context: ctx, at: at, done: done}, func() {
		t.Stop()
		cancel(nil)
	}
}

// deadline is the context of WithTimeout.
type deadline struct {
	context.Context
	at   time.Time
	done <-chan struct{}
}

// Done closes when the deadline, parent or cancellation ends the context.
func (c deadline) Done() <-chan struct{} { return c.done }

// Deadline is when the context ends on the clock, or its parent's earlier
// deadline.
func (c deadline) Deadline() (time.Time, bool) {
	if at, ok := c.Context.Deadline(); ok && at.Before(c.at) {
		return at, true
	}
	return c.at, true
}

// Err is context.DeadlineExceeded once the clock passed the deadline, else
// the parent's or the cancellation's error.
func (c deadline) Err() error {
	err := c.Context.Err()
	if err != nil {
		<-c.done
	}
	if err != nil && errors.Is(context.Cause(c.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return err
}
