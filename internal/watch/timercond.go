package watch

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

// RunProbe reports whether a probe command exits 0; a seam for the tests.
var RunProbe = probePasses

// TimerFire is an auto timer the watch fired: what it does once the state
// is written.
type TimerFire struct {
	T        state.Timer
	TimedOut bool
	// reason is why it fired: "due 11:05", "<condition> holds", "timed out …".
	Reason string
	// late is how late a timer fired past its time and why, or empty
	// (timerLate); a command it runs reads it from BEEKEEPER_TIMER_LATE.
	Late string
	// nextDue is the repeating occurrence saved when this run was claimed.
	NextDue time.Time
}

// Message is what a woken agent reads.
func (f TimerFire) Message() string {
	return fmt.Sprintf("beekeeper timer #%d (%s): %s", f.T.ID, f.Reason, f.T.What)
}

// SettleTimers fires the auto timers of st due at now: a plain one at its
// time, a conditional one once found says it holds and, past its Until, as
// timed out, or closed unfired when it expires; a fired or expired timer is
// closed, a repeating one re-armed at its next time. A check that found the
// condition not holding is recorded with its reason; a reference that could
// not be read or a probe that could not run is a line once, until the reason
// changes, and a condition that has not held for timerStuckAfter is a line
// once. A probe that could not run is also the guide's report once for the
// checks in a row that fail so, closed by the next clean check. It returns
// the lines, events and fires, and whether it changed st.
func SettleTimers(st *state.State, found map[int]CheckResult, now time.Time) ([]string, []state.Event, []TimerFire, bool) {
	return Cmd.SettleTimersMatching(st, found, now, timerTick, nil)
}

// wouldFireTimers says, for --once, what the auto timers of st would do at
// now, found their checks: each line as a running watch would say it, marked
// that --once writes nothing. It settles a copy; st and its timers stay as
// they are, so a look by hand neither wakes nor closes another's timer.
func (w *Watcher) wouldFireTimers(st *state.State, found map[int]CheckResult) {
	scratch := state.State{Timers: slices.Clone(st.Timers), Stop: st.Stop}
	lines, _, _, _ := SettleTimers(&scratch, found, w.Host.Now())
	for _, l := range lines {
		if head, rest, ok := strings.Cut(l, ": "); ok {
			l = head + " (--once writes nothing): " + rest
		}
		w.EmitNow("pending", "%s", l)
	}
}

// ActTimers wakes the agents and runs the commands of the fired timers,
// outside the poll: a wake by name takes seconds, a command up to
// timerRunTimeout. A timer without either was its watch line.
func (w *Watcher) ActTimers(ctx context.Context, fires []TimerFire) {
	var wakeFires []TimerFire
	for _, f := range fires {
		switch {
		case f.T.Wake != "":
			wakeFires = append(wakeFires, f)
		case f.T.Run != "":
			w.TimerActs.Go(func() { w.RunTimer(ctx, f) })
		}
	}
	if len(wakeFires) > 0 {
		w.TimerActs.Go(func() { w.wakeTimers(ctx, wakeFires) })
	}
}

type TimerWakeOutcome struct {
	Fire    TimerFire
	Rearm   bool
	CloseCI bool
	Line    string
}

// WakeTimer wakes a fired timer's agent with its message; a wake that fails
// is a line, so the supervisor's watch still has the text.
func (w *Watcher) WakeTimer(ctx context.Context, f TimerFire) {
	w.wakeTimers(ctx, []TimerFire{f})
}

func (w *Watcher) wakeTimers(ctx context.Context, fires []TimerFire) {
	if len(fires) == 0 {
		return
	}
	// A timer may have fired before the agent replaced its wait. Read the
	// current park at delivery, including parks off the roster; a stale
	// repeating timer must never turn a session waiting on a person.
	st, err := w.Host.Store().Read()
	if err != nil {
		outcomes := make([]TimerWakeOutcome, 0, len(fires))
		for _, f := range fires {
			outcomes = append(outcomes, TimerWakeOutcome{
				Fire:  f,
				Rearm: true,
				Line:  fmt.Sprintf("TIMER WAKE FAILED: #%d could not read the current park (%v)", f.T.ID, err),
			})
		}
		w.settleWakeOutcomes(outcomes)
		return
	}
	type wakeDelivery struct {
		original TimerFire
		resolved TimerFire
	}
	var immediate []TimerWakeOutcome
	var deliveries []wakeDelivery
	for _, f := range fires {
		original := f
		if Cmd.EndedTimerTarget(st, f.T.Wake) {
			immediate = append(immediate, TimerWakeOutcome{Fire: original, CloseCI: true})
			continue
		}
		if strings.EqualFold(f.T.Wake, guideRoleAlias) {
			h := Cmd.GuideRole().Get(st).Holder
			if h == nil {
				immediate = append(immediate, TimerWakeOutcome{
					Fire:  original,
					Rearm: true,
					Line:  fmt.Sprintf("TIMER WAKE FAILED: #%d: no session holds the guide role", f.T.ID),
				})
				continue
			}
			f.T.Wake = h.Session
			if f.T.Wake == "" {
				f.T.Wake = h.Name
			}
		}
		if !f.T.WakeParkAt.IsZero() {
			p := Cmd.WakeTargetPark(st, f.T.Wake)
			if p == nil || !p.Checks || !p.At.Equal(f.T.WakeParkAt) || !p.By.Is(f.T.By) {
				immediate = append(immediate, TimerWakeOutcome{
					Fire:    original,
					CloseCI: true,
				})
				continue // The worker has already left or replaced this CI wait.
			}
		}
		if p := Cmd.WakeTargetPark(st, f.T.Wake); p != nil && !TimerSettlesPark(f.T, p, w.Host.Cfg().Guide.Person) {
			immediate = append(immediate, TimerWakeOutcome{
				Fire:  original,
				Rearm: true,
				Line:  fmt.Sprintf("TIMER WAKE SKIPPED: #%d for %q: %s; its current wake condition has not fired", f.T.ID, f.T.Wake, Cmd.ParkedText(p, w.Host.Now())),
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
	outcomes := make([]TimerWakeOutcome, len(deliveries))
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
func (w *Watcher) deliverWakeTimer(ctx context.Context, st *state.State, original TimerFire, f TimerFire) TimerWakeOutcome {
	return Cmd.WatchDeliverWakeTimer(w, ctx, st, original, f)
}

func (w *Watcher) timerRetryNow() time.Time {
	if w.Clock != nil || w.Polls.Load() > 0 || w.Host.Now().IsZero() {
		return w.Clk().Now().UTC()
	}
	return w.Host.Now().UTC()
}

func (w *Watcher) settleWakeOutcomes(outcomes []TimerWakeOutcome) {
	needStateWrite := false
	var lines []string
	for _, o := range outcomes {
		if o.Rearm || (o.CloseCI && !o.Fire.T.WakeParkAt.IsZero()) {
			needStateWrite = true
		}
		if o.Line != "" {
			lines = append(lines, o.Line)
		}
	}
	if !needStateWrite {
		if len(lines) > 0 {
			w.emitLinesNow("timer", lines)
		}
		return
	}
	err := w.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		checkedAt := w.timerRetryNow()
		var evs []state.Event
		for _, o := range outcomes {
			if o.Line != "" {
				evs = append(evs, Cmd.Event(WatchParty, "watch.timer", "%s", o.Line))
			}
			if o.CloseCI && !o.Fire.T.WakeParkAt.IsZero() {
				st.Timers = slices.DeleteFunc(st.Timers, func(current state.Timer) bool { return current.ID == o.Fire.T.ID })
			}
			if o.Rearm {
				evs = append(evs, RearmTimerInState(st, o.Fire, "wake not delivered; retrying", checkedAt))
			}
		}
		return evs, nil
	})
	if err != nil {
		var errLines []string
		for _, o := range outcomes {
			if o.Line != "" {
				errLines = append(errLines, o.Line)
			}
			if o.CloseCI && !o.Fire.T.WakeParkAt.IsZero() {
				errLines = append(errLines, fmt.Sprintf("TIMER WAKE CLEANUP FAILED: #%d: %v", o.Fire.T.ID, err))
			}
			if o.Rearm {
				errLines = append(errLines, fmt.Sprintf("TIMER WAKE RETRY FAILED: #%d: %v", o.Fire.T.ID, err))
			}
		}
		w.emitLinesNow("timer", errLines)
		return
	}
	for _, line := range lines {
		w.OutputEvent(line)
	}
}

func RearmTimerInState(st *state.State, f TimerFire, held string, checkedAt time.Time) state.Event {
	retry := f.T
	retry.Checked, retry.Fired = time.Time{}, time.Time{}
	retry.Held = held
	// Space all delivery retries, including timers without a condition.
	if held != "previous action interrupted" {
		retry.Checked = checkedAt
		retry.PendingWakeReason, retry.PendingWakeTimedOut = f.Reason, f.TimedOut
		// This occurrence already fired; expiry must not discard its retry.
		retry.Expire = false
	}
	for i, t := range st.Timers {
		if t.ID != retry.ID {
			continue
		}
		if t.Repeat != "" && t.Run == retry.Run && t.Wake == retry.Wake && t.Due.Equal(f.NextDue) {
			st.Timers[i] = retry
		}
		return Cmd.Event(WatchParty, "timer.failed", "#%d %s: %s", retry.ID, held, retry.What)
	}
	st.Timers = append(st.Timers, retry)
	return Cmd.Event(WatchParty, "timer.failed", "#%d %s; re-armed: %s", retry.ID, held, retry.What)
}

// RunTimer runs a fired timer's command, its output in timers/<id>.log of
// the state directory, and logs and says its exit code.
func (w *Watcher) RunTimer(ctx context.Context, f TimerFire) {
	path := filepath.Join(w.Host.Store().Dir(), "timers", fmt.Sprintf("%d.log", f.T.ID))
	rctx, cancel := context.WithTimeout(ctx, timerRunTimeout)
	defer cancel()
	c := proc.CommandContext(rctx, "sh", "-c", f.T.Run) //nolint:gosec // the command its timer's setter gave on this machine
	Cmd.MergeCheckGroup(c)                              // cancellation ends the shell and its child commands
	// An agents start the command runs says which timer started it, its
	// time and how late it fired (agentStartTimer).
	c.Env = append(os.Environ(), envTimerCommand+"=run", envTimerID+"="+strconv.Itoa(f.T.ID), envTimerDue+"="+f.T.Due.UTC().Format(time.RFC3339), envTimerLate+"="+f.Late)
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
			w.EmitNow("timer", "TIMER FAILED: #%d `%s` interrupted (%v); could not re-arm (%v): %s", f.T.ID, f.T.Run, runErr, err, f.T.What)
		} else if rearmed {
			w.EmitNow("timer", "TIMER FAILED: #%d `%s` interrupted (%v); timer re-armed: %s", f.T.ID, f.T.Run, runErr, f.T.What)
		} else {
			w.EmitNow("timer", "TIMER FAILED: #%d `%s` interrupted (%v); current timer retained: %s", f.T.ID, f.T.Run, runErr, f.T.What)
		}
		return
	}
	_ = w.Host.Store().Log(Cmd.Event(WatchParty, "timer.ran", "#%d exit %d (%s): %s", f.T.ID, rc, path, f.T.Run))
	w.EmitNow("timer", "TIMER RAN: #%d exit %d, output in %s: %s", f.T.ID, rc, path, Cmd.Truncate(f.T.What, 200))
}

// Retain the interrupted occurrence without overwriting a changed timer.
func (w *Watcher) rearmInterruptedTimer(f TimerFire) (bool, error) {
	return w.RearmTimer(f, "previous action interrupted")
}

// RearmTimer restores an undelivered occurrence without replacing a newer edit.
func (w *Watcher) RearmTimer(f TimerFire, held string) (bool, error) {
	rearmed := false
	err := w.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		retry := f.T
		retry.Checked, retry.Fired = time.Time{}, time.Time{}
		retry.Held = held
		// Space all delivery retries, including timers without a condition.
		if held != "previous action interrupted" {
			// Delivery can finish long after the dispatch snapshot was taken.
			// Start the retry interval at this attempt, not at that old tick.
			retry.Checked = w.Clk().Now().UTC()
			retry.PendingWakeReason, retry.PendingWakeTimedOut = f.Reason, f.TimedOut
			// This occurrence already fired; expiry must not discard its retry.
			retry.Expire = false
		}
		for i, t := range st.Timers {
			if t.ID != retry.ID {
				continue
			}
			if t.Repeat != "" && t.Run == retry.Run && t.Wake == retry.Wake && t.Due.Equal(f.NextDue) {
				st.Timers[i] = retry
				rearmed = true
			}
			return []state.Event{Cmd.Event(WatchParty, "timer.failed", "#%d %s: %s", retry.ID, held, retry.What)}, nil
		}
		st.Timers = append(st.Timers, retry)
		rearmed = true
		return []state.Event{Cmd.Event(WatchParty, "timer.failed", "#%d %s; re-armed: %s", retry.ID, held, retry.What)}, nil
	})
	return rearmed, err
}
