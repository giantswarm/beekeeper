package clock

import (
	"sync"
	"time"
)

// Queue is a keyed timer queue on a Clock: scheduling a key again replaces its timer. The engine keeps
// every deadline here (wait deadlines, re-observe backoff, resyncs, freshness checks, retries, budget
// resets, retention) and wakes controllers from the callbacks.
type Queue struct {
	clock Clock
	mu    sync.Mutex
	items map[string]queued
	// Stopped, when it returns true for a key, keeps that key's callback from running (test faults).
	Stopped func(key string) bool
}

type queued struct {
	at    time.Time
	timer Timer
}

// NewQueue returns a queue on c.
func NewQueue(c Clock) *Queue { return &Queue{clock: c, items: map[string]queued{}} }

// Schedule runs f at at (at once when at is not after now), replacing any timer for key.
func (q *Queue) Schedule(key string, at time.Time, f func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if old, ok := q.items[key]; ok {
		old.timer.Stop()
	}
	d := at.Sub(q.clock.Now())
	var t Timer
	t = q.clock.AfterFunc(d, func() {
		q.mu.Lock()
		cur, ok := q.items[key]
		if !ok || cur.timer != t {
			q.mu.Unlock()
			return
		}
		delete(q.items, key)
		stopped := q.Stopped
		q.mu.Unlock()
		if stopped != nil && stopped(key) {
			return
		}
		f()
	})
	q.items[key] = queued{at: at, timer: t}
}

// Cancel removes key's timer.
func (q *Queue) Cancel(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if old, ok := q.items[key]; ok {
		old.timer.Stop()
		delete(q.items, key)
	}
}

// At returns when key is scheduled.
func (q *Queue) At(key string) (time.Time, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	it, ok := q.items[key]
	return it.at, ok
}

// Len returns the number of scheduled keys.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// CancelAll removes every timer (on losing leadership or stopping).
func (q *Queue) CancelAll() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for k, it := range q.items {
		it.timer.Stop()
		delete(q.items, k)
	}
}
