package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// timerEvery is how often the watch checks a timer's condition by default.
const timerEvery = 5 * time.Minute

// timerStuckAfter is how long a condition may fail to hold before the watch
// says so: a probe that never exits 0 may read the wrong field.
const timerStuckAfter = 24 * time.Hour

// timerRunTimeout bounds a timer's --run command.
const timerRunTimeout = 30 * time.Minute

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

// runProbe reports whether a probe command exits 0; a seam for the tests.
var runProbe = probePasses

// checkTimers runs the checks of the conditional timers whose check is due
// at now, each distinct command once however many timers share it, and
// returns by timer id what it found. A GitHub condition waits while the
// budget is under the floor (lowBudget), and while the merges queued for its
// reset go first (resetFirst).
func checkTimers(ctx context.Context, timers []state.Timer, now time.Time, lowBudget bool) map[int]checkResult {
	checks := map[string]timerCheck{}
	byCmd := map[string][]int{}
	for _, t := range timers {
		if !t.Conditional() || t.Due.After(now) || !t.Checked.IsZero() && now.Sub(t.Checked) < cmp.Or(t.Every, timerEvery) {
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

// timerFire is an auto timer the watch fired: what it does once the state
// is written.
type timerFire struct {
	t        state.Timer
	timedOut bool
	// reason is why it fired: "due 11:05", "<condition> holds", "timed out …".
	reason string
	// late is how late a timer fired past its time and why, or empty
	// (timerLate); a command it runs reads it from BEEKEEPER_TIMER_LATE.
	late string
	// nextDue is the repeating occurrence saved when this run was claimed.
	nextDue time.Time
}

// message is what a woken agent reads.
func (f timerFire) message() string {
	return fmt.Sprintf("beekeeper timer #%d (%s): %s", f.t.ID, f.reason, f.t.What)
}

// settleTimers fires the auto timers of st due at now: a plain one at its
// time, a conditional one once found says it holds and, past its Until, as
// timed out, or closed unfired when it expires; a fired or expired timer is
// closed, a repeating one re-armed at its next time. A check that found the
// condition not holding is recorded with its reason; a reference that could
// not be read is a line once, until the reason changes, and a condition that
// has not held for timerStuckAfter is a line once. It returns the
// lines, events and fires, and whether it changed st.
func settleTimers(st *state.State, found map[int]checkResult, now time.Time) ([]string, []state.Event, []timerFire, bool) {
	return settleTimersMatching(st, found, now, timerTick, nil)
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
		r, checked := found[t.ID]
		over := !t.Until.IsZero() && !now.Before(t.Until)
		f := timerFire{t: t}
		switch {
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
		case over && t.Expire:
			changed = true
			lines = append(lines, fmt.Sprintf("TIMER EXPIRED: #%d, %s did not hold by %s%s: %s", t.ID, timerCond(t), clock(now, t.Until), lastFound(t), truncate(t.What, 200)))
			evs = append(evs, event(watchParty, "timer.expired", "#%d %s: %s", t.ID, timerCond(t), t.What))
			continue
		case over:
			f.timedOut, f.reason = true, fmt.Sprintf("timed out at %s, %s did not hold%s", clock(now, t.Until), timerCond(t), lastFound(t))
		default:
			if checked {
				if r.unreadable && (!t.Unreadable || t.Reason != r.reason) {
					lines = append(lines, fmt.Sprintf("TIMER UNREADABLE: #%d, %s cannot be read (%s); it keeps waiting: %s", t.ID, timerCond(t), r.reason, truncate(t.What, 200)))
					evs = append(evs, event(watchParty, "timer.unreadable", "#%d %s: %s", t.ID, timerCond(t), r.reason))
				}
				t.Checked, t.Reason, t.Unreadable, changed = now.UTC(), r.reason, r.unreadable, true
				if t.Since.IsZero() {
					t.Since = now.UTC()
				}
				if !t.Stuck && now.Sub(t.Since) >= timerStuckAfter {
					t.Stuck = true
					lines = append(lines, fmt.Sprintf("TIMER STUCK: #%d, %s has not held since %s%s; it keeps waiting: %s", t.ID, timerCond(t), clock(now, t.Since), lastFound(t), truncate(t.What, 200)))
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

// wouldFireTimers says, for --once, what the auto timers of st would do at
// now, found their checks: each line as a running watch would say it, marked
// that --once writes nothing. It settles a copy; st and its timers stay as
// they are, so a look by hand neither wakes nor closes another's timer.
func (w *watcher) wouldFireTimers(st *state.State, found map[int]checkResult) {
	scratch := state.State{Timers: slices.Clone(st.Timers), Stop: st.Stop}
	lines, _, _, _ := settleTimers(&scratch, found, w.now)
	for _, l := range lines {
		if head, rest, ok := strings.Cut(l, ": "); ok {
			l = head + " (--once writes nothing): " + rest
		}
		w.emitNow("pending", "%s", l)
	}
}

// actTimers wakes the agents and runs the commands of the fired timers,
// outside the poll: a wake by name takes seconds, a command up to
// timerRunTimeout. A timer without either was its watch line.
func (w *watcher) actTimers(ctx context.Context, fires []timerFire) {
	for _, f := range fires {
		switch {
		case f.t.Wake != "":
			w.timerActs.Go(func() { w.wakeTimer(ctx, f) })
		case f.t.Run != "":
			w.timerActs.Go(func() { w.runTimer(ctx, f) })
		}
	}
}

// wakeTimer wakes a fired timer's agent with its message; a wake that fails
// is a line, so the supervisor's watch still has the text.
func (w *watcher) wakeTimer(ctx context.Context, f timerFire) {
	// A timer may have fired before the agent replaced its wait. Read the
	// current park at delivery, including parks off the roster; a stale
	// repeating timer must never turn a session waiting on a person.
	st, err := w.store.Read()
	if err != nil {
		w.emitNow("timer", "TIMER WAKE FAILED: #%d could not read the current park (%v)", f.t.ID, err)
		return
	}
	if strings.EqualFold(f.t.Wake, guideRoleAlias) {
		h := guideRole.get(st).Holder
		if h == nil {
			w.emitNow("timer", "TIMER WAKE FAILED: #%d: no session holds the guide role", f.t.ID)
			return
		}
		f.t.Wake = h.Session
		if f.t.Wake == "" {
			f.t.Wake = h.Name
		}
	}
	if !f.t.WakeParkAt.IsZero() {
		p := wakeTargetPark(st, f.t.Wake)
		if p == nil || !p.Checks || !p.At.Equal(f.t.WakeParkAt) || !p.By.Is(f.t.By) {
			w.closeCIWakeTimer(f.t)
			return // The worker has already left or replaced this CI wait.
		}
	}
	if p := wakeTargetPark(st, f.t.Wake); p != nil && !timerSettlesPark(f.t, p, w.cfg.Guide.Person) {
		w.emitNow("timer", "TIMER WAKE SKIPPED: #%d for %q: %s; its current wake condition has not fired", f.t.ID, f.t.Wake, parkedText(p, w.now))
		return
	}
	quiet := *w.app // the wake's own report is not a watch line
	quiet.out = io.Discard
	if !f.t.Due.IsZero() {
		quiet.executionRequestID = fmt.Sprintf("timer/%d/%s/%s", f.t.ID, f.t.Due.UTC().Format(time.RFC3339Nano), execution.Digest([]byte(f.message())))
	}
	// A CI park belongs to this wake until the execution harness accepts
	// it. Pass the park through the durable delivery ledger so acceptance
	// clears it, while storage or an uncertain delivery keeps it parked.
	var wakeErr error
	if i, findErr := findAgent(st, f.t.Wake); findErr == nil && st.Agents[i].Execution != nil && st.Agents[i].Park != nil && st.Agents[i].Park.Checks {
		ag := st.Agents[i]
		wakeErr = quiet.enqueueExecutionWake(ctx, watchParty, ag, f.message(), ag.Park)
	} else {
		wakeErr = wakeOwner(&quiet, ctx, watchParty, f.t.Wake, f.message(), "")
	}
	if err := wakeErr; err != nil {
		var pending *executionDeliveryPending
		if errors.As(err, &pending) {
			w.closeCIWakeTimer(f.t)
			w.emitNow("timer", "TIMER WAKE QUEUED: #%d for %q; delivery pending: %s", f.t.ID, f.t.Wake, truncate(f.t.What, 200))
			return
		}
		w.emitNow("timer", "TIMER WAKE FAILED: #%d could not wake %q (%v): %s", f.t.ID, f.t.Wake, err, truncate(f.t.What, 200))
		return
	}
	w.closeCIWakeTimer(f.t)
}

// closeCIWakeTimer retires a CI timer only after durable handoff or obsolescence.
func (w *watcher) closeCIWakeTimer(t state.Timer) {
	if t.WakeParkAt.IsZero() {
		return
	}
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Timers = slices.DeleteFunc(st.Timers, func(current state.Timer) bool { return current.ID == t.ID })
		return nil, nil
	}); err != nil {
		w.emitNow("timer", "TIMER WAKE CLEANUP FAILED: #%d: %v", t.ID, err)
	}
}

// runTimer runs a fired timer's command, its output in timers/<id>.log of
// the state directory, and logs and says its exit code.
func (w *watcher) runTimer(ctx context.Context, f timerFire) {
	path := filepath.Join(w.store.Dir(), "timers", fmt.Sprintf("%d.log", f.t.ID))
	rctx, cancel := context.WithTimeout(ctx, timerRunTimeout)
	defer cancel()
	c := exec.CommandContext(rctx, "sh", "-c", f.t.Run) //nolint:gosec // the command its timer's setter gave on this machine
	// An agents start the command runs says which timer started it, its
	// time and how late it fired (agentStartTimer).
	c.Env = append(os.Environ(), envTimerID+"="+strconv.Itoa(f.t.ID), envTimerDue+"="+f.t.Due.UTC().Format(time.RFC3339), envTimerLate+"="+f.late)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		if out, err := os.Create(path); err == nil { //nolint:gosec // under the state directory, named by the timer id
			defer func() { _ = out.Close() }()
			c.Stdout, c.Stderr = out, out
		}
	}
	rc := 0
	runErr := c.Run()
	if runErr != nil {
		rc = -1
		if c.ProcessState != nil {
			rc = c.ProcessState.ExitCode()
		}
	}
	if runErr != nil && (rctx.Err() != nil || c.ProcessState != nil && rc == -1) {
		// Shutdown waits for timerActs before handing over to another watch.
		// Restore the due occurrence before that handover, including a
		// repeating timer whose next occurrence was already scheduled.
		rearmed := false
		err := w.store.Update(func(st *state.State) ([]state.Event, error) {
			retry := f.t
			retry.Checked, retry.Fired = time.Time{}, time.Time{}
			retry.Held = "previous command interrupted"
			for i, t := range st.Timers {
				if t.ID != retry.ID {
					continue
				}
				if t.Repeat != "" && t.Run == retry.Run && t.Due.Equal(f.nextDue) {
					st.Timers[i] = retry
					rearmed = true
				}
				return []state.Event{event(watchParty, "timer.failed", "#%d interrupted: %s", retry.ID, retry.Run)}, nil
			}
			st.Timers = append(st.Timers, retry)
			rearmed = true
			return []state.Event{event(watchParty, "timer.failed", "#%d interrupted; re-armed: %s", retry.ID, retry.Run)}, nil
		})
		if err != nil {
			w.emitNow("timer", "TIMER FAILED: #%d `%s` interrupted (%v); could not re-arm (%v): %s", f.t.ID, f.t.Run, runErr, err, f.t.What)
		} else if rearmed {
			w.emitNow("timer", "TIMER FAILED: #%d `%s` interrupted (%v); timer re-armed: %s", f.t.ID, f.t.Run, runErr, f.t.What)
		} else {
			w.emitNow("timer", "TIMER FAILED: #%d `%s` interrupted (%v); current timer retained: %s", f.t.ID, f.t.Run, runErr, f.t.What)
		}
		return
	}
	_ = w.store.Log(event(watchParty, "timer.ran", "#%d exit %d (%s): %s", f.t.ID, rc, path, f.t.Run))
	w.emitNow("timer", "TIMER RAN: #%d exit %d, output in %s: %s", f.t.ID, rc, path, truncate(f.t.What, 200))
}
