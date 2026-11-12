// Package clock is beekeeper's time: Now, Since, Until, After, Sleep,
// NewTimer, NewTicker and AfterFunc, in place of the time package's.
//
// A release build uses the system clock and nothing else: every function is
// the time package's own, and nothing reads the environment.
//
// A scenario build (go build -tags scenario, docs/scenarios.md) reads Env.
// When it names a socket, every process of the scenario reads the scenario
// world's time from it: the system clock moved by the world's offset. The
// world's advance step moves that offset forward and wakes, in every
// process, the timers, tickers and sleeps then due, so an hour of loop
// passes takes seconds. Without Env a scenario build uses the system clock
// too.
//
// The core flows use this package instead of the time package's clock
// functions; TestMigratedPackagesUseTheClock lists them and fails when one
// of them calls the time package's clock again.
package clock

import "time"

// Env names the scenario world's clock socket to a scenario build. A
// release build never reads it.
const Env = "BEEKEEPER_SCENARIO_CLOCK"

// timer is what a Timer stops and resets: a *time.Timer, or the scenario
// clock's.
type timer interface {
	Stop() bool
	Reset(d time.Duration) bool
}

// Timer is time.Timer on the clock: C receives the time once it fires,
// nil for one of AfterFunc.
type Timer struct {
	C <-chan time.Time
	t timer
}

// Stop stops the timer; false when it had fired or been stopped already.
// As with time.Timer since Go 1.23, no stale time is received after it.
func (t *Timer) Stop() bool { return t.t.Stop() }

// Reset makes the timer fire after d; false when it had fired or been
// stopped already.
func (t *Timer) Reset(d time.Duration) bool { return t.t.Reset(d) }

// ticker is what a Ticker stops and resets: a *time.Ticker, or the
// scenario clock's.
type ticker interface {
	Stop()
	Reset(d time.Duration)
}

// Ticker is time.Ticker on the clock: C receives the time every period,
// dropping ticks a slow receiver misses.
type Ticker struct {
	C <-chan time.Time
	t ticker
}

// Stop stops the ticker; C receives no tick after it.
func (t *Ticker) Stop() { t.t.Stop() }

// Reset stops the ticker and restarts it with period d.
func (t *Ticker) Reset(d time.Duration) { t.t.Reset(d) }

// The system clock: a release build's only clock, and a scenario build's
// without a world.

func systemTimer(d time.Duration) *Timer {
	t := time.NewTimer(d)
	return &Timer{C: t.C, t: t}
}

func systemTicker(d time.Duration) *Ticker {
	t := time.NewTicker(d)
	return &Ticker{C: t.C, t: t}
}

func systemAfterFunc(d time.Duration, f func()) *Timer {
	return &Timer{t: time.AfterFunc(d, f)}
}
