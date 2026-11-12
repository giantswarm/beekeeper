//go:build scenario

package clock

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Scenario says this build reads a scenario world's clock (Env).
const Scenario = true

var (
	worldOnce sync.Once
	worldTime *remote
)

// world is the scenario world's clock Env names, connected on first use;
// nil without Env. A world that cannot be reached ends the process: a
// scenario must never run on another clock than its world's.
func world() *remote {
	worldOnce.Do(func() {
		path := os.Getenv(Env)
		if path == "" {
			return
		}
		r, err := dial(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "beekeeper: scenario clock %s unreachable: %v\n", path, err)
			os.Exit(125)
		}
		worldTime = r
	})
	return worldTime
}

// Now is the current time: the world's in a scenario.
func Now() time.Time {
	if r := world(); r != nil {
		return r.now()
	}
	return time.Now()
}

// Since is the time elapsed since t.
func Since(t time.Time) time.Duration { return Now().Sub(t) }

// Until is the duration until t.
func Until(t time.Time) time.Duration { return t.Sub(Now()) }

// After receives the time once d has passed.
func After(d time.Duration) <-chan time.Time { return NewTimer(d).C }

// Sleep pauses for at least d.
func Sleep(d time.Duration) {
	if r := world(); r != nil {
		r.sleep(d)
		return
	}
	time.Sleep(d)
}

// NewTimer is a Timer that fires after d.
func NewTimer(d time.Duration) *Timer {
	if r := world(); r != nil {
		return r.newTimer(d, nil)
	}
	return systemTimer(d)
}

// NewTicker is a Ticker of period d, which must be positive.
func NewTicker(d time.Duration) *Ticker {
	if d <= 0 {
		panic("clock: non-positive interval for NewTicker")
	}
	if r := world(); r != nil {
		return r.newTicker(d)
	}
	return systemTicker(d)
}

// AfterFunc calls f in its own goroutine once d has passed.
func AfterFunc(d time.Duration, f func()) *Timer {
	if r := world(); r != nil {
		return r.newTimer(d, f)
	}
	return systemAfterFunc(d, f)
}
