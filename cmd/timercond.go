package cmd

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// timerEvery is how often the watch checks a timer's condition by default.
const timerEvery = 5 * time.Minute

// timerStuckAfter is how long a condition may fail to hold before the watch
// says so: a probe that never exits 0 may read the wrong field.
const timerStuckAfter = 24 * time.Hour

// timerRunTimeout bounds a timer's --run command.
const timerRunTimeout = 30 * time.Minute

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
// budget is under the floor (lowBudget).
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

// timerFire is an auto timer the watch fired: what it does once the state
// is written.
type timerFire struct {
	t        state.Timer
	timedOut bool
	// reason is why it fired: "due 11:05", "<condition> holds", "timed out …".
	reason string
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
	var lines []string
	var evs []state.Event
	var fires []timerFire
	changed := false
	kept := st.Timers[:0]
	for _, t := range st.Timers {
		if !t.Auto() || t.Due.After(now) {
			kept = append(kept, t)
			continue
		}
		r, checked := found[t.ID]
		over := !t.Until.IsZero() && !now.Before(t.Until)
		f := timerFire{t: t}
		switch {
		case !t.Conditional():
			f.reason = "due " + clock(now, t.Due)
		case r.holds:
			f.reason = timerCond(t) + " holds"
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
		fires = append(fires, f)
		var act string
		switch {
		case t.Wake != "":
			act = fmt.Sprintf("waking %q: ", t.Wake)
		case t.Run != "":
			act = fmt.Sprintf("running `%s`: ", truncate(t.Run, 80))
		}
		if t.Repeat != "" {
			t.Due, t.Checked = t.Next(now), time.Time{}
			act += "next " + clock(now, t.Due) + ": "
			kept = append(kept, t)
		}
		lines = append(lines, fmt.Sprintf("TIMER: #%d, %s: %s%s", t.ID, f.reason, act, truncate(t.What, 200)))
		evs = append(evs, event(watchParty, "timer.fired", "#%d %s: %s%s", t.ID, f.reason, act, t.What))
	}
	clear(st.Timers[len(kept):])
	st.Timers = kept
	return lines, evs, fires, changed
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
	quiet := *w.app // the wake's own report is not a watch line
	quiet.out = io.Discard
	if err := wakeOwner(&quiet, ctx, watchParty, f.t.Wake, f.message(), ""); err != nil {
		w.emitNow("timer", "TIMER WAKE FAILED: #%d could not wake %q (%v): %s", f.t.ID, f.t.Wake, err, truncate(f.t.What, 200))
	}
}

// runTimer runs a fired timer's command, its output in timers/<id>.log of
// the state directory, and logs and says its exit code.
func (w *watcher) runTimer(ctx context.Context, f timerFire) {
	path := filepath.Join(w.store.Dir(), "timers", fmt.Sprintf("%d.log", f.t.ID))
	rctx, cancel := context.WithTimeout(ctx, timerRunTimeout)
	defer cancel()
	c := exec.CommandContext(rctx, "sh", "-c", f.t.Run) //nolint:gosec // the command its timer's setter gave on this machine
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		if out, err := os.Create(path); err == nil { //nolint:gosec // under the state directory, named by the timer id
			defer func() { _ = out.Close() }()
			c.Stdout, c.Stderr = out, out
		}
	}
	rc := 0
	if err := c.Run(); err != nil {
		rc = -1
		if c.ProcessState != nil {
			rc = c.ProcessState.ExitCode()
		}
	}
	_ = w.store.Log(event(watchParty, "timer.ran", "#%d exit %d (%s): %s", f.t.ID, rc, path, f.t.Run))
	w.emitNow("timer", "TIMER RAN: #%d exit %d, output in %s: %s", f.t.ID, rc, path, truncate(f.t.What, 200))
}
