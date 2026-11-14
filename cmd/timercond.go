package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// timerEvery is how often the watch checks a timer's condition by default.
const timerEvery = 5 * time.Minute

// timerStuckAfter is how long a condition may fail to hold before the watch
// says so: a probe that never exits 0 may read the wrong field.
const timerStuckAfter = 24 * time.Hour

// timerTick is how long a tick of the watch or the supervisor loop is by
// default (watch.interval, supervisor.loopInterval): a timer firing more
// than one tick past its time fired late.
const timerTick = 30 * time.Second

// timerHeldDown is why a timer fired late that nothing on record held: no
// tick of the watch or the supervisor loop settled the state between its
// time and its firing.
const timerHeldDown = "no watch or supervisor loop tick settled it since its time (down, or the state locked)"

// timerLate says how late a timer firing at now is, and what held it, when
// it fires more than one tick past the tick that should have fired it:
// "14m late: <what held it>". It is empty for a timer on time.
func timerLate(t state.Timer, now time.Time, tick time.Duration) string {
	late := now.Sub(t.Due)
	if !lateBy(late, tick) {
		return ""
	}
	return dur(late) + " late: " + cmp.Or(t.Held, timerHeldDown)
}

// lateBy says whether late is more than one tick past the tick that should
// have acted at the due time: a tick's own slack is on time.
func lateBy(late, tick time.Duration) bool { return late > 2*max(tick, timerTick) }

// lastFound is what t's last check found, for a line: " (<reason>)", or
// empty.
func lastFound(t state.Timer) string {
	if t.Reason == "" {
		return ""
	}
	return " (" + truncate(t.Reason, 200) + ")"
}

// timerCond names t's condition in a line.
func timerCond(t state.Timer) string {
	if t.When != "" {
		return t.When
	}
	return "probe `" + truncate(t.Probe, 80) + "`"
}

// unchecked says why a check of t's condition found no answer: its probe
// could not run, its reference cannot be read.
func unchecked(t state.Timer) string {
	if t.Probe != "" {
		return "could not run"
	}
	return "cannot be read"
}

// notHeld says how t's condition failed to hold by its end: a clean check
// found it false, or the last check found no answer (unchecked).
func notHeld(t state.Timer) string {
	switch {
	case !t.Unreadable:
		return "did not hold"
	case t.Probe != "":
		return "could not run"
	}
	return "could not be read"
}

// notHeldSince says how long t's condition has not held, for its TIMER
// STUCK line: a probe that could not run since Failing was not seen to hold,
// which says nothing of its condition.
func notHeldSince(now time.Time, t state.Timer) string {
	if t.Failing.IsZero() {
		return "has not held since " + clock(now, t.Since)
	}
	return fmt.Sprintf("has not been seen to hold since %s, could not run since %s", clock(now, t.Since), clock(now, t.Failing))
}

// timerProbeKey keys the guide's report of t's checks whose probe could not
// run, by the first of them.
func timerProbeKey(t state.Timer) string {
	return fmt.Sprintf("timer-probe #%d %s", t.ID, t.Failing.UTC().Format(time.RFC3339Nano))
}

// timerProbeFailed is the guide's report that t's probe could not run at
// now: once for the checks in a row that fail so, with the first one's
// reason.
func timerProbeFailed(t state.Timer, now time.Time) state.Event {
	var acts string
	switch {
	case t.Wake != "":
		acts = fmt.Sprintf(" to wake %q", t.Wake)
	case t.Run != "":
		acts = fmt.Sprintf(" to run `%s`", truncate(t.Run, 80))
	}
	finding := fmt.Sprintf("TIMER PROBE FAILED: #%d, %s could not run at %s (%s): its condition is unknown, not false. The timer keeps waiting%s: %s",
		t.ID, timerCond(t), clockZone(now, now), t.Reason, acts, truncate(t.What, 200))
	return event(watchParty, loop.LoopFileVerb, "[%s] %s reported at %s%s%s",
		timerProbeKey(t), watchParty.Name, now.UTC().Format(time.RFC3339), loop.FileReportSeparator, finding)
}

// timerProbeRecovered closes the guide's report of t's probe once a check
// runs it again at now.
func timerProbeRecovered(t state.Timer, now time.Time) state.Event {
	return event(watchParty, loop.LoopFileVerb, "[%s] %s reported at %s%sRECOVERED: timer #%d's probe runs again at %s",
		timerProbeKey(t), watchParty.Name, now.UTC().Format(time.RFC3339), loop.FileReportSeparator, t.ID, clockZone(now, now))
}

// checkTimers runs the checks of the conditional timers whose check is due
// at now, each distinct command once however many timers share it, and
// returns by timer id what it found. A GitHub condition waits while the
// budget is under the floor (lowBudget), and while the merges queued for its
// reset go first (resetFirst).
func checkTimers(ctx context.Context, timers []state.Timer, now time.Time, lowBudget bool) map[int]checkResult {
	checks := map[string]timerCheck{}
	byCmd := map[string][]int{}
	for _, t := range timers {
		if !t.Conditional() || t.PendingWakeReason != "" || t.Due.After(now) || !t.Checked.IsZero() && now.Sub(t.Checked) < cmp.Or(t.Every, timerEvery) {
			continue
		}
		if c := checkOf(t); c.cmd != "" && (!c.github || !lowBudget) {
			checks[c.cmd] = c
			byCmd[c.cmd] = append(byCmd[c.cmd], t.ID)
		}
	}
	found := map[int]checkResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for cmd, ids := range byCmd {
		wg.Go(func() {
			r := checks[cmd].result(readProbe(ctx, cmd))
			mu.Lock()
			defer mu.Unlock()
			for _, id := range ids {
				found[id] = r
			}
		})
	}
	wg.Wait()
	return found
}

// lowBudget reports whether the last GitHub budget reading is under the
// floor until its reset.
func lowBudget(b *state.Budget, floor int, now time.Time) bool {
	return b != nil && b.Remaining < floor && now.Before(b.Reset)
}

// resetFirst reports whether merges queued under the floor for the GitHub
// budget's reset go first now (merge.resetFirst).
func (a *app) resetFirst(st *state.State) bool {
	return len(merge.ResetFirst(st, a.now, a.cfg.Merge.ResetFirst.Duration, proc.Alive)) > 0
}

// timerActs distinguishes wakes and commands from watch-only reminders.
func timerActs(t state.Timer) bool { return t.Wake != "" || t.Run != "" }

// settleTimersMatching leaves timers rejected by accept untouched. A nil
// predicate accepts every timer, as watch does. tick is the caller's own
// interval: a timer at its time fires more than one tick late only when
// something held it, which its firing says (timerLate).
func settleTimersMatching(st *state.State, found map[int]checkResult, now time.Time, tick time.Duration, accept func(state.Timer) bool) ([]string, []state.Event, []timerFire, bool) {
	var lines []string
	var evs []state.Event
	var fires []timerFire
	changed := false
	kept := st.Timers[:0]
	for _, t := range st.Timers {
		if endedTimerTarget(st, t.Wake) {
			changed = true
			evs = append(evs, event(watchParty, "timer.cancelled", "#%d: wake target ended: %s", t.ID, t.Wake))
			continue
		}
		if (accept != nil && !accept(t)) || !t.Auto() || t.Due.After(now) {
			kept = append(kept, t)
			continue
		}
		if st.Stop != nil && t.Wake != "" {
			// A wake timer due while beekeeper is stopped fires after the
			// resume, and says the stop held it.
			if t.Held == "" {
				t.Held, changed = "held by the beekeeper stop since "+clockZone(now, st.Stop.At), true
			}
			kept = append(kept, t)
			continue
		}
		if t.PendingWakeReason != "" && !t.Checked.IsZero() && now.Sub(t.Checked) < cmp.Or(t.Every, timerEvery) {
			kept = append(kept, t)
			continue
		}
		r, checked := found[t.ID]
		over := !t.Until.IsZero() && !now.Before(t.Until)
		f := timerFire{t: t}
		switch {
		case t.PendingWakeReason != "":
			f.reason, f.timedOut = t.PendingWakeReason, t.PendingWakeTimedOut
		case !t.Conditional():
			f.reason = "due " + clockZone(now, t.Due)
			if f.late = timerLate(t, now, tick); f.late != "" {
				f.reason += ", fired " + clockZone(now, now) + ", " + f.late
			}
		case r.holds:
			// A condition that says how it settled (pr-checks: green or
			// red) tells the woken agent.
			f.reason = timerCond(t) + " holds"
			if r.reason != "" {
				f.reason += ": " + r.reason
			}
			if !t.Failing.IsZero() {
				evs = append(evs, timerProbeRecovered(t, now))
				// A wake not delivered re-arms the timer from its firing
				// (rearmTimerInState): its probe ran there too.
				t.Failing, f.t.Failing = time.Time{}, time.Time{}
			}
		case over && t.Expire:
			changed = true
			lines = append(lines, fmt.Sprintf("TIMER EXPIRED: #%d, %s %s by %s%s: %s", t.ID, timerCond(t), notHeld(t), clock(now, t.Until), lastFound(t), truncate(t.What, 200)))
			evs = append(evs, event(watchParty, "timer.expired", "#%d %s: %s", t.ID, timerCond(t), t.What))
			continue
		case over:
			f.timedOut, f.reason = true, fmt.Sprintf("timed out at %s, %s %s%s", clock(now, t.Until), timerCond(t), notHeld(t), lastFound(t))
		default:
			if checked {
				if r.unreadable && (!t.Unreadable || t.Reason != r.reason) {
					lines = append(lines, fmt.Sprintf("TIMER UNREADABLE: #%d, %s %s (%s); it keeps waiting: %s", t.ID, timerCond(t), unchecked(t), r.reason, truncate(t.What, 200)))
					evs = append(evs, event(watchParty, "timer.unreadable", "#%d %s: %s", t.ID, timerCond(t), r.reason))
				}
				t.Checked, t.Reason, t.Unreadable, changed = now.UTC(), r.reason, r.unreadable, true
				// A probe that could not run is the guide's to hear of once
				// for the checks in a row that fail so; a clean check ends
				// them. Only a clean false is "not yet".
				switch {
				case t.Probe != "" && r.unreadable && t.Failing.IsZero():
					t.Failing = now.UTC()
					evs = append(evs, timerProbeFailed(t, now))
				case !r.unreadable && !t.Failing.IsZero():
					evs = append(evs, timerProbeRecovered(t, now))
					t.Failing = time.Time{}
				}
				if t.Since.IsZero() {
					t.Since = now.UTC()
				}
				if !t.Stuck && now.Sub(t.Since) >= timerStuckAfter {
					t.Stuck = true
					lines = append(lines, fmt.Sprintf("TIMER STUCK: #%d, %s %s%s; it keeps waiting: %s", t.ID, timerCond(t), notHeldSince(now, t), lastFound(t), truncate(t.What, 200)))
					evs = append(evs, event(watchParty, "timer.stuck", "#%d %s since %s: %s", t.ID, timerCond(t), t.Since.Format(time.RFC3339), t.Reason))
				}
			}
			kept = append(kept, t)
			continue
		}
		changed = true
		var act string
		switch {
		case t.Wake != "":
			act = fmt.Sprintf("waking %q: ", t.Wake)
		case t.Run != "":
			act = fmt.Sprintf("running `%s`: ", truncate(t.Run, 80))
		}
		if !t.WakeParkAt.IsZero() {
			// Keep CI wakes durable across a restart before outbox insertion.
			kept = append(kept, t)
		} else if t.Repeat != "" {
			t.Due, t.Checked, t.Held = t.Next(now), time.Time{}, ""
			t.PendingWakeReason, t.PendingWakeTimedOut = "", false
			f.nextDue = t.Due
			act += "next " + clock(now, t.Due) + ": "
			kept = append(kept, t)
		}
		fires = append(fires, f)
		lines = append(lines, fmt.Sprintf("TIMER: #%d, %s: %s%s", t.ID, f.reason, act, truncate(t.What, 200)))
		evs = append(evs, event(watchParty, "timer.fired", "#%d %s: %s%s", t.ID, f.reason, act, t.What))
	}
	clear(st.Timers[len(kept):])
	st.Timers = kept
	return lines, evs, fires, changed
}

func watchDeliverWakeTimer(w *watcher, ctx context.Context, st *state.State, original, f timerFire) timerWakeOutcome {
	if i, err := findAgent(st, f.t.Wake); err == nil && freshPark(st, st.Agents[i]) {
		ag := st.Agents[i]
		// The loop owns CI, budget retries, approval and merge for a
		// proven review request; reminders need no worker turn.
		if rp, ok := st.ReviewParkFor(ag.Park.On); ok && rp.Party.Is(ag.Party) && ag.Park.ReviewHead != "" && rp.ReviewHead == ag.Park.ReviewHead {
			return timerWakeOutcome{fire: original, closeCI: true}
		}
		handed, err := w.handCITimerToReview(ctx, f, ag)
		if err != nil {
			return timerWakeOutcome{
				fire: original, rearm: true,
				line: fmt.Sprintf("TIMER HANDOFF FAILED: #%d: %v", f.t.ID, err),
			}
		}
		if !handed {
			quiet := *w.a
			quiet.out = io.Discard
			if err := quiet.queueContinuation(watchParty, ag, f.message()); err != nil {
				return timerWakeOutcome{
					fire: original, rearm: true,
					line: fmt.Sprintf("TIMER CONTINUATION FAILED: #%d: %v", f.t.ID, err),
				}
			}
		}
		return timerWakeOutcome{fire: original, closeCI: true}
	}

	quiet := *w.a // the wake's own report is not a watch line
	quiet.out = io.Discard
	if !f.t.Due.IsZero() {
		quiet.executionRequestID = fmt.Sprintf("timer/%d/%s/%s", f.t.ID, f.t.Due.UTC().Format(time.RFC3339Nano), execution.Digest([]byte(f.message())))
	}
	// A CI park belongs to this wake until the execution harness accepts
	// it. Pass the park through the durable delivery ledger so acceptance
	// clears it, while storage or an uncertain delivery keeps it parked.
	var wakeErr error
	if i, findErr := findAgent(st, f.t.Wake); findErr == nil && !freshPark(st, st.Agents[i]) && st.Agents[i].Execution != nil && st.Agents[i].Park != nil && st.Agents[i].Park.Checks {
		ag := st.Agents[i]
		wakeErr = quiet.enqueueExecutionWake(ctx, watchParty, ag, f.message(), ag.Park)
	} else {
		wakeErr = wakeOwner(&quiet, ctx, watchParty, f.t.Wake, f.message(), "")
	}
	if err := wakeErr; err != nil {
		var pending *executionDeliveryPending
		if errors.As(err, &pending) {
			return timerWakeOutcome{
				fire:    original,
				closeCI: !f.t.WakeParkAt.IsZero(),
				line:    fmt.Sprintf("TIMER WAKE QUEUED: #%d for %q; delivery pending: %s", f.t.ID, f.t.Wake, truncate(f.t.What, 200)),
			}
		}
		return timerWakeOutcome{
			fire:  original,
			rearm: true,
			line:  fmt.Sprintf("TIMER WAKE FAILED: #%d could not wake %q (%v): %s", f.t.ID, f.t.Wake, err, truncate(f.t.What, 200)),
		}
	}
	return timerWakeOutcome{
		fire:    original,
		closeCI: !f.t.WakeParkAt.IsZero(),
	}
}
