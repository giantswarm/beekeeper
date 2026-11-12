//go:build !scenario

package clock

import "time"

// Scenario says this build reads a scenario world's clock (Env); a release
// build never does.
const Scenario = false

// Now is the current time.
func Now() time.Time { return time.Now() }

// Since is the time elapsed since t.
func Since(t time.Time) time.Duration { return time.Since(t) }

// Until is the duration until t.
func Until(t time.Time) time.Duration { return time.Until(t) }

// After receives the time once d has passed.
func After(d time.Duration) <-chan time.Time { return time.After(d) }

// Sleep pauses for at least d.
func Sleep(d time.Duration) { time.Sleep(d) }

// NewTimer is a Timer that fires after d.
func NewTimer(d time.Duration) *Timer { return systemTimer(d) }

// NewTicker is a Ticker of period d, which must be positive.
func NewTicker(d time.Duration) *Ticker { return systemTicker(d) }

// AfterFunc calls f in its own goroutine once d has passed.
func AfterFunc(d time.Duration, f func()) *Timer { return systemAfterFunc(d, f) }
