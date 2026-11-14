// Package clock is the engine's only source of time. Production uses Real; scenarios use Fake, which
// moves only when told to. Engine code never calls time.Now, time.Sleep, time.After or RequeueAfter.
package clock

import (
	"sort"
	"sync"
	"time"
)

// Clock tells the time and runs functions later.
type Clock interface {
	Now() time.Time
	// AfterFunc runs f in its own goroutine (Real) or synchronously while the clock is advanced (Fake)
	// once d has passed.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a pending AfterFunc.
type Timer interface {
	// Stop prevents the function from running; it reports whether it did.
	Stop() bool
}

// Real is the system clock.
type Real struct{}

// Now returns the current UTC time.
func (Real) Now() time.Time { return time.Now().UTC() }

// AfterFunc wraps time.AfterFunc.
func (Real) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// Fake is a clock that moves only when Set or Advance is called. Due functions run synchronously, in
// deadline order (ties in scheduling order), on the goroutine that moves the clock, with Now() equal to
// their deadline.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	seq    uint64
	timers []*fakeTimer
}

type fakeTimer struct {
	c   *Fake
	at  time.Time
	seq uint64
	f   func()
}

// NewFake returns a fake clock at t.
func NewFake(t time.Time) *Fake { return &Fake{now: t.UTC()} }

// Now returns the fake time.
func (c *Fake) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// AfterFunc schedules f at Now()+d. A non-positive d is due at once but still runs only when the clock
// is next moved (use Advance(0) or Fire to run it).
func (c *Fake) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	t := &fakeTimer{c: c, at: c.now.Add(d), seq: c.seq, f: f}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	c := t.c
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, x := range c.timers {
		if x == t {
			c.timers = append(c.timers[:i], c.timers[i+1:]...)
			return true
		}
	}
	return false
}

// NextDeadline returns the earliest pending timer's deadline.
func (c *Fake) NextDeadline() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.timers) == 0 {
		return time.Time{}, false
	}
	next := c.timers[0].at
	for _, t := range c.timers[1:] {
		if t.at.Before(next) {
			next = t.at
		}
	}
	return next, true
}

// Pending returns the number of pending timers.
func (c *Fake) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// Set moves the clock to t (never backwards) and runs every function due at or before t, each at its
// own deadline. Functions scheduled by those functions run too if they are due by t.
func (c *Fake) Set(t time.Time) {
	for c.fireOne(t) {
	}
	c.mu.Lock()
	if t.After(c.now) {
		c.now = t.UTC()
	}
	c.mu.Unlock()
}

// Advance moves the clock by d.
func (c *Fake) Advance(d time.Duration) { c.Set(c.Now().Add(d)) }

// Step runs the functions due at the earliest deadline at or before limit, moving the clock there; it
// reports false when nothing is due by limit. The harness settles the engine after each Step.
func (c *Fake) Step(limit time.Time) bool {
	next, ok := c.NextDeadline()
	if !ok || next.After(limit) {
		return false
	}
	for c.fireAt(next) {
	}
	return true
}

// fireAt runs one timer due exactly at or before at, moving the clock to its deadline.
func (c *Fake) fireAt(at time.Time) bool { return c.fireOne(at) }

func (c *Fake) fireOne(limit time.Time) bool {
	c.mu.Lock()
	if len(c.timers) == 0 {
		c.mu.Unlock()
		return false
	}
	sort.SliceStable(c.timers, func(i, j int) bool {
		if c.timers[i].at.Equal(c.timers[j].at) {
			return c.timers[i].seq < c.timers[j].seq
		}
		return c.timers[i].at.Before(c.timers[j].at)
	})
	t := c.timers[0]
	if t.at.After(limit) {
		c.mu.Unlock()
		return false
	}
	c.timers = c.timers[1:]
	if t.at.After(c.now) {
		c.now = t.at
	}
	c.mu.Unlock()
	t.f()
	return true
}
