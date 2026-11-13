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
// not be read or a probe that could not run is a line once, until the reason
// changes, and a condition that has not held for timerStuckAfter is a line
// once. A probe that could not run is also the guide's report once for the
// checks in a row that fail so, closed by the next clean check. It returns
// the lines, events and fires, and whether it changed st.
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
				t.Failing = time.Time{}
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
	var wakeFires []timerFire
	for _, f := range fires {
		switch {
		case f.t.Wake != "":
			wakeFires = append(wakeFires, f)
		case f.t.Run != "":
			w.timerActs.Go(func() { w.runTimer(ctx, f) })
		}
	}
	if len(wakeFires) > 0 {
		w.timerActs.Go(func() { w.wakeTimers(ctx, wakeFires) })
	}
}

type timerWakeOutcome struct {
	fire    timerFire
	rearm   bool
	closeCI bool
	line    string
}

// wakeTimer wakes a fired timer's agent with its message; a wake that fails
// is a line, so the supervisor's watch still has the text.
func (w *watcher) wakeTimer(ctx context.Context, f timerFire) {
	w.wakeTimers(ctx, []timerFire{f})
}

func (w *watcher) wakeTimers(ctx context.Context, fires []timerFire) {
	if len(fires) == 0 {
		return
	}
	// A timer may have fired before the agent replaced its wait. Read the
	// current park at delivery, including parks off the roster; a stale
	// repeating timer must never turn a session waiting on a person.
	st, err := w.store.Read()
	if err != nil {
		outcomes := make([]timerWakeOutcome, 0, len(fires))
		for _, f := range fires {
			outcomes = append(outcomes, timerWakeOutcome{
				fire:  f,
				rearm: true,
				line:  fmt.Sprintf("TIMER WAKE FAILED: #%d could not read the current park (%v)", f.t.ID, err),
			})
		}
		w.settleWakeOutcomes(outcomes)
		return
	}
	type wakeDelivery struct {
		original timerFire
		resolved timerFire
	}
	var immediate []timerWakeOutcome
	var deliveries []wakeDelivery
	for _, f := range fires {
		original := f
		if endedTimerTarget(st, f.t.Wake) {
			immediate = append(immediate, timerWakeOutcome{fire: original, closeCI: true})
			continue
		}
		if strings.EqualFold(f.t.Wake, guideRoleAlias) {
			h := guideRole.get(st).Holder
			if h == nil {
				immediate = append(immediate, timerWakeOutcome{
					fire:  original,
					rearm: true,
					line:  fmt.Sprintf("TIMER WAKE FAILED: #%d: no session holds the guide role", f.t.ID),
				})
				continue
			}
			f.t.Wake = h.Session
			if f.t.Wake == "" {
				f.t.Wake = h.Name
			}
		}
		if !f.t.WakeParkAt.IsZero() {
			p := wakeTargetPark(st, f.t.Wake)
			if p == nil || !p.Checks || !p.At.Equal(f.t.WakeParkAt) || !p.By.Is(f.t.By) {
				immediate = append(immediate, timerWakeOutcome{
					fire:    original,
					closeCI: true,
				})
				continue // The worker has already left or replaced this CI wait.
			}
		}
		if p := wakeTargetPark(st, f.t.Wake); p != nil && !timerSettlesPark(f.t, p, w.cfg.Guide.Person) {
			immediate = append(immediate, timerWakeOutcome{
				fire:  original,
				rearm: true,
				line:  fmt.Sprintf("TIMER WAKE SKIPPED: #%d for %q: %s; its current wake condition has not fired", f.t.ID, f.t.Wake, parkedText(p, w.now)),
			})
			continue
		}
		deliveries = append(deliveries, wakeDelivery{original: original, resolved: f})
	}
	if len(immediate) > 0 {
		w.settleWakeOutcomes(immediate)
	}
	if len(deliveries) == 0 {
		return
	}
	outcomes := make([]timerWakeOutcome, len(deliveries))
	var wg sync.WaitGroup
	for idx, d := range deliveries {
		wg.Go(func() {
			outcomes[idx] = w.deliverWakeTimer(ctx, st, d.original, d.resolved)
		})
	}
	wg.Wait()
	w.settleWakeOutcomes(outcomes)
}

func (w *watcher) deliverWakeTimer(ctx context.Context, st *state.State, original, f timerFire) timerWakeOutcome {
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
			quiet := *w.app
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

	quiet := *w.app // the wake's own report is not a watch line
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

func (w *watcher) timerRetryNow() time.Time {
	if w.clock != nil || w.polls.Load() > 0 || w.now.IsZero() {
		return w.clk().Now().UTC()
	}
	return w.now.UTC()
}

func (w *watcher) settleWakeOutcomes(outcomes []timerWakeOutcome) {
	needStateWrite := false
	var lines []string
	for _, o := range outcomes {
		if o.rearm || (o.closeCI && !o.fire.t.WakeParkAt.IsZero()) {
			needStateWrite = true
		}
		if o.line != "" {
			lines = append(lines, o.line)
		}
	}
	if !needStateWrite {
		if len(lines) > 0 {
			w.emitLinesNow("timer", lines)
		}
		return
	}
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		checkedAt := w.timerRetryNow()
		var evs []state.Event
		for _, o := range outcomes {
			if o.line != "" {
				evs = append(evs, event(watchParty, "watch.timer", "%s", o.line))
			}
			if o.closeCI && !o.fire.t.WakeParkAt.IsZero() {
				st.Timers = slices.DeleteFunc(st.Timers, func(current state.Timer) bool { return current.ID == o.fire.t.ID })
			}
			if o.rearm {
				evs = append(evs, rearmTimerInState(st, o.fire, "wake not delivered; retrying", checkedAt))
			}
		}
		return evs, nil
	})
	if err != nil {
		var errLines []string
		for _, o := range outcomes {
			if o.line != "" {
				errLines = append(errLines, o.line)
			}
			if o.closeCI && !o.fire.t.WakeParkAt.IsZero() {
				errLines = append(errLines, fmt.Sprintf("TIMER WAKE CLEANUP FAILED: #%d: %v", o.fire.t.ID, err))
			}
			if o.rearm {
				errLines = append(errLines, fmt.Sprintf("TIMER WAKE RETRY FAILED: #%d: %v", o.fire.t.ID, err))
			}
		}
		w.emitLinesNow("timer", errLines)
		return
	}
	for _, line := range lines {
		w.outputEvent(line)
	}
}

func rearmTimerInState(st *state.State, f timerFire, held string, checkedAt time.Time) state.Event {
	retry := f.t
	retry.Checked, retry.Fired = time.Time{}, time.Time{}
	retry.Held = held
	// Space all delivery retries, including timers without a condition.
	if held != "previous action interrupted" {
		retry.Checked = checkedAt
		retry.PendingWakeReason, retry.PendingWakeTimedOut = f.reason, f.timedOut
		// This occurrence already fired; expiry must not discard its retry.
		retry.Expire = false
	}
	for i, t := range st.Timers {
		if t.ID != retry.ID {
			continue
		}
		if t.Repeat != "" && t.Run == retry.Run && t.Wake == retry.Wake && t.Due.Equal(f.nextDue) {
			st.Timers[i] = retry
		}
		return event(watchParty, "timer.failed", "#%d %s: %s", retry.ID, held, retry.What)
	}
	st.Timers = append(st.Timers, retry)
	return event(watchParty, "timer.failed", "#%d %s; re-armed: %s", retry.ID, held, retry.What)
}

// runTimer runs a fired timer's command, its output in timers/<id>.log of
// the state directory, and logs and says its exit code.
func (w *watcher) runTimer(ctx context.Context, f timerFire) {
	path := filepath.Join(w.store.Dir(), "timers", fmt.Sprintf("%d.log", f.t.ID))
	rctx, cancel := context.WithTimeout(ctx, timerRunTimeout)
	defer cancel()
	c := exec.CommandContext(rctx, "sh", "-c", f.t.Run) //nolint:gosec // the command its timer's setter gave on this machine
	mergeCheckGroup(c)                                  // cancellation ends the shell and its child commands
	// An agents start the command runs says which timer started it, its
	// time and how late it fired (agentStartTimer).
	c.Env = append(os.Environ(), envTimerCommand+"=run", envTimerID+"="+strconv.Itoa(f.t.ID), envTimerDue+"="+f.t.Due.UTC().Format(time.RFC3339), envTimerLate+"="+f.late)
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
		rearmed, err := w.rearmInterruptedTimer(f)
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

// Retain the interrupted occurrence without overwriting a changed timer.
func (w *watcher) rearmInterruptedTimer(f timerFire) (bool, error) {
	return w.rearmTimer(f, "previous action interrupted")
}

// rearmTimer restores an undelivered occurrence without replacing a newer edit.
func (w *watcher) rearmTimer(f timerFire, held string) (bool, error) {
	rearmed := false
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		retry := f.t
		retry.Checked, retry.Fired = time.Time{}, time.Time{}
		retry.Held = held
		// Space all delivery retries, including timers without a condition.
		if held != "previous action interrupted" {
			// Delivery can finish long after the dispatch snapshot was taken.
			// Start the retry interval at this attempt, not at that old tick.
			retry.Checked = w.clk().Now().UTC()
			retry.PendingWakeReason, retry.PendingWakeTimedOut = f.reason, f.timedOut
			// This occurrence already fired; expiry must not discard its retry.
			retry.Expire = false
		}
		for i, t := range st.Timers {
			if t.ID != retry.ID {
				continue
			}
			if t.Repeat != "" && t.Run == retry.Run && t.Wake == retry.Wake && t.Due.Equal(f.nextDue) {
				st.Timers[i] = retry
				rearmed = true
			}
			return []state.Event{event(watchParty, "timer.failed", "#%d %s: %s", retry.ID, held, retry.What)}, nil
		}
		st.Timers = append(st.Timers, retry)
		rearmed = true
		return []state.Event{event(watchParty, "timer.failed", "#%d %s; re-armed: %s", retry.ID, held, retry.What)}, nil
	})
	return rearmed, err
}
