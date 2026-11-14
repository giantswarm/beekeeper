// Package faults holds the engine bugs scenarios inject so the invariants that catch them can be seen
// firing (contract §4.2, engine_fault). In production every flag stays false.
package faults

import "sync/atomic"

// Faults are the injected engine bugs.
type Faults struct {
	// StopTimers stops wait-deadline timers only (resyncs and freshness go on).
	StopTimers atomic.Bool
	// StopDecisions stops team decision passes.
	StopDecisions atomic.Bool
	// StopFreshness stops freshness checks.
	StopFreshness atomic.Bool
	// DropWaits stops creating waits.
	DropWaits atomic.Bool
	// ForgetRefusals forgets recorded refusals (so refused reads are retried on unchanged inputs).
	ForgetRefusals atomic.Bool
	// GateEverything makes every gate hold.
	GateEverything atomic.Bool

	// OnResume, when set, runs after Resume (the engine fires overdue timers at once).
	OnResume func()
}

// Set switches a fault on by its scenario name; it reports false for an unknown name.
func (f *Faults) Set(kind string) bool {
	switch kind {
	case "stop_timers":
		f.StopTimers.Store(true)
	case "stop_decisions":
		f.StopDecisions.Store(true)
	case "stop_freshness":
		f.StopFreshness.Store(true)
	case "drop_waits":
		f.DropWaits.Store(true)
	case "forget_refusals":
		f.ForgetRefusals.Store(true)
	case "gate_everything":
		f.GateEverything.Store(true)
	case "resume":
		f.Resume()
	default:
		return false
	}
	return true
}

// Resume removes every injected bug.
func (f *Faults) Resume() {
	f.StopTimers.Store(false)
	f.StopDecisions.Store(false)
	f.StopFreshness.Store(false)
	f.DropWaits.Store(false)
	f.ForgetRefusals.Store(false)
	f.GateEverything.Store(false)
	if f.OnResume != nil {
		f.OnResume()
	}
}
