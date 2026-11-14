package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// timerRunTimeout bounds a timer's --run command.
const timerRunTimeout = 30 * time.Minute

// runProbe reports whether a probe command exits 0; a seam for the tests.
var runProbe = probePasses

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

// wouldFireTimers says, for --once, what the auto timers of st would do at
// now, found their checks: each line as a running watch would say it, marked
// that --once writes nothing. It settles a copy; st and its timers stay as
// they are, so a look by hand neither wakes nor closes another's timer.
func (w *watcher) wouldFireTimers(st *state.State, found map[int]checkResult) {
	scratch := state.State{Timers: slices.Clone(st.Timers), Stop: st.Stop}
	lines, _, _, _ := settleTimers(&scratch, found, w.a.now)
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
	st, err := w.a.store.Read()
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
		if p := wakeTargetPark(st, f.t.Wake); p != nil && !timerSettlesPark(f.t, p, w.a.cfg.Guide.Person) {
			immediate = append(immediate, timerWakeOutcome{
				fire:  original,
				rearm: true,
				line:  fmt.Sprintf("TIMER WAKE SKIPPED: #%d for %q: %s; its current wake condition has not fired", f.t.ID, f.t.Wake, parkedText(p, w.a.now)),
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

// deliverWakeTimer runs in the command, which holds what it needs (watchDeliverWakeTimer).
func (w *watcher) deliverWakeTimer(ctx context.Context, st *state.State, original timerFire, f timerFire) timerWakeOutcome {
	return watchDeliverWakeTimer(w, ctx, st, original, f)
}

func (w *watcher) timerRetryNow() time.Time {
	if w.clock != nil || w.polls.Load() > 0 || w.a.now.IsZero() {
		return w.clk().Now().UTC()
	}
	return w.a.now.UTC()
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
	err := w.a.store.Update(func(st *state.State) ([]state.Event, error) {
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
	path := filepath.Join(w.a.store.Dir(), "timers", fmt.Sprintf("%d.log", f.t.ID))
	rctx, cancel := context.WithTimeout(ctx, timerRunTimeout)
	defer cancel()
	c := proc.CommandContext(rctx, "sh", "-c", f.t.Run) //nolint:gosec // the command its timer's setter gave on this machine
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
	_ = w.a.store.Log(event(watchParty, "timer.ran", "#%d exit %d (%s): %s", f.t.ID, rc, path, f.t.Run))
	w.emitNow("timer", "TIMER RAN: #%d exit %d, output in %s: %s", f.t.ID, rc, path, truncate(f.t.What, 200))
}

// Retain the interrupted occurrence without overwriting a changed timer.
func (w *watcher) rearmInterruptedTimer(f timerFire) (bool, error) {
	return w.rearmTimer(f, "previous action interrupted")
}

// rearmTimer restores an undelivered occurrence without replacing a newer edit.
func (w *watcher) rearmTimer(f timerFire, held string) (bool, error) {
	rearmed := false
	err := w.a.store.Update(func(st *state.State) ([]state.Event, error) {
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
