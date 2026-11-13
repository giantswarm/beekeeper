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
	return deadline{Context: ctx, at: at}, func() {
		t.Stop()
		cancel(nil)
	}
}

// deadline is the context of WithTimeout.
type deadline struct {
	context.Context
	at time.Time
}

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
	if err != nil && errors.Is(context.Cause(c.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return err
}
