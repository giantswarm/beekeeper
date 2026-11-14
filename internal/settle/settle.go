// Package settle tracks every asynchronous unit of work in the engine and its fakes, so a test can
// wait for exact idleness instead of a quiet window (contract §3.3). Production code calls it too; there
// it costs an atomic add.
package settle

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Tracker counts work in progress and asks probes whether anything else is pending.
type Tracker struct {
	mu     sync.Mutex
	busy   map[string]int
	probes map[string]Probe
	change chan struct{}
}

// Probe reports whether a component is idle, and what is pending when it isn't.
type Probe func() (idle bool, pending string)

// New returns an empty tracker.
func New() *Tracker {
	return &Tracker{busy: map[string]int{}, probes: map[string]Probe{}, change: make(chan struct{})}
}

// Begin marks one unit of work (named for reports) as started; call the returned func when it is done.
// A nil Tracker is valid and tracks nothing.
func (t *Tracker) Begin(what string) (done func()) {
	if t == nil {
		return func() {}
	}
	t.mu.Lock()
	t.busy[what]++
	t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			if t.busy[what]--; t.busy[what] <= 0 {
				delete(t.busy, what)
			}
			t.signal()
			t.mu.Unlock()
		})
	}
}

// AddProbe registers a probe under name, replacing one of the same name.
func (t *Tracker) AddProbe(name string, p Probe) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.probes[name] = p
	t.mu.Unlock()
}

// RemoveProbe removes a probe.
func (t *Tracker) RemoveProbe(name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.probes, name)
	t.mu.Unlock()
}

// Poke wakes Wait to re-check the probes (call it when a probed condition changes).
func (t *Tracker) Poke() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.signal()
	t.mu.Unlock()
}

func (t *Tracker) signal() {
	close(t.change)
	t.change = make(chan struct{})
}

// Idle reports whether nothing is busy, and lists what is pending otherwise.
func (t *Tracker) Idle() (bool, []string) {
	t.mu.Lock()
	var pending []string
	for what, n := range t.busy {
		pending = append(pending, fmt.Sprintf("%s (%d)", what, n))
	}
	probes := make(map[string]Probe, len(t.probes))
	for k, p := range t.probes {
		probes[k] = p
	}
	t.mu.Unlock()
	for name, p := range probes {
		if idle, what := p(); !idle {
			pending = append(pending, name+": "+what)
		}
	}
	sort.Strings(pending)
	return len(pending) == 0, pending
}

// Wait blocks until Idle holds twice in a row with nothing finishing in between, or the timeout passes.
//
// The probes are asked one after another, so work can move from a probe not yet asked to one already
// asked (a reconcile that enqueues the next one and then finishes) and every probe still answers idle.
// Whatever finishes signals (a Begin's done, a Poke), so a scan during which nothing signalled saw a
// consistent state: nothing can start without something busy starting it.
func (t *Tracker) Wait(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		t.mu.Lock()
		ch := t.change
		t.mu.Unlock()
		idle, pending := t.Idle()
		if idle {
			// Re-check once after yielding: a unit may start between the probes.
			time.Sleep(200 * time.Microsecond)
			if idle2, _ := t.Idle(); idle2 && !signalled(ch) {
				return nil
			}
			continue
		}
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("engine did not settle within %s; still busy: %v", timeout, pending)
		}
		poll := time.Millisecond
		if left < poll {
			poll = left
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		case <-time.After(poll):
		}
	}
}

// signalled reports whether ch, a change channel taken earlier, was closed since: something finished.
func signalled(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
