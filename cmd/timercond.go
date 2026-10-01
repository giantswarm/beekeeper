package cmd

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// timerEvery is how often the watch checks a timer's condition by default.
const timerEvery = 5 * time.Minute

// timerRunTimeout bounds a timer's --run command.
const timerRunTimeout = 30 * time.Minute

// timerCondition is a typed condition of timer add --when: the reference it
// names (form, matched by ref) and the probe that exits 0 once it holds.
type timerCondition struct {
	form   string
	ref    *regexp.Regexp
	github bool
	probe  func(m []string) string
}

var (
	ghRef   = regexp.MustCompile(`^([\w.-]+/[\w.-]+)#(\d+)$`)
	kubeRef = regexp.MustCompile(`^([\w.@:-]+)/([a-z0-9][a-z0-9.-]*)/([a-z0-9][a-z0-9.-]*)$`)
)

// timerConditions are the typed conditions by kind. Each probe is one read
// of one reference, so the timers on the same reference share it.
var timerConditions = map[string]timerCondition{
	"pr-merged": {"owner/repo#n", ghRef, true, func(m []string) string {
		return fmt.Sprintf("gh api repos/%s/pulls/%s --jq .merged | grep -qx true", m[1], m[2])
	}},
	"issue-closed": {"owner/repo#n", ghRef, true, func(m []string) string {
		return fmt.Sprintf("gh api repos/%s/issues/%s --jq .state | grep -qx closed", m[1], m[2])
	}},
	"helmrelease-ready": {"context/namespace/name", kubeRef, false, func(m []string) string {
		return fmt.Sprintf(`kubectl --context %s -n %s get helmrelease %s -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' | grep -qx True`, m[1], m[2], m[3])
	}},
	"controlplane-ready": {"context/namespace/name", kubeRef, false, func(m []string) string {
		return fmt.Sprintf(`kubectl --context %s -n %s get kubeadmcontrolplane %s -o jsonpath='{.spec.replicas}/{.status.readyReplicas}/{.status.updatedReplicas}' | grep -Eqx '([0-9]+)/\1/\1'`, m[1], m[2], m[3])
	}},
}

// conditionProbe is the probe of a typed condition "<kind> <ref>" and
// whether it reads GitHub.
func conditionProbe(when string) (string, bool, error) {
	kind, ref, _ := strings.Cut(strings.TrimSpace(when), " ")
	c, ok := timerConditions[kind]
	if !ok {
		return "", false, usageErr("--when %q: a condition is one of %s, then its reference; --probe takes a command", when, strings.Join(slices.Sorted(maps.Keys(timerConditions)), ", "))
	}
	m := c.ref.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return "", false, usageErr("--when %q: %s names %s", when, kind, c.form)
	}
	return c.probe(m), c.github, nil
}

// timerProbe is the command that checks t's condition and whether it reads
// GitHub; empty for a timer without one.
func timerProbe(t state.Timer) (string, bool) {
	if t.Probe != "" {
		return t.Probe, false
	}
	if t.When == "" {
		return "", false
	}
	cmd, github, err := conditionProbe(t.When)
	if err != nil {
		return "", false
	}
	return cmd, github
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

// checkTimers runs the probes of the conditional timers whose check is due
// at now, each distinct command once however many timers share it, and
// returns by timer id whether its condition holds. A GitHub condition waits
// while the budget is under the floor (lowBudget).
func checkTimers(ctx context.Context, timers []state.Timer, now time.Time, lowBudget bool) map[int]bool {
	byCmd := map[string][]int{}
	for _, t := range timers {
		if !t.Conditional() || t.Due.After(now) || !t.Checked.IsZero() && now.Sub(t.Checked) < cmp.Or(t.Every, timerEvery) {
			continue
		}
		if cmd, github := timerProbe(t); cmd != "" && (!github || !lowBudget) {
			byCmd[cmd] = append(byCmd[cmd], t.ID)
		}
	}
	held := map[int]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for cmd, ids := range byCmd {
		wg.Go(func() {
			ok := runProbe(ctx, cmd)
			mu.Lock()
			defer mu.Unlock()
			for _, id := range ids {
				held[id] = ok
			}
		})
	}
	wg.Wait()
	return held
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
// time, a conditional one once held says it holds and, past its Until, as
// timed out, or closed unfired when it expires; a fired or expired timer is
// closed. A check that found the condition not holding is recorded. It
// returns the lines, events and fires, and whether it changed st.
func settleTimers(st *state.State, held map[int]bool, now time.Time) ([]string, []state.Event, []timerFire, bool) {
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
		h, checked := held[t.ID]
		over := !t.Until.IsZero() && !now.Before(t.Until)
		f := timerFire{t: t}
		switch {
		case !t.Conditional():
			f.reason = "due " + clock(now, t.Due)
		case h:
			f.reason = timerCond(t) + " holds"
		case over && t.Expire:
			changed = true
			lines = append(lines, fmt.Sprintf("TIMER EXPIRED: #%d, %s did not hold by %s: %s", t.ID, timerCond(t), clock(now, t.Until), truncate(t.What, 200)))
			evs = append(evs, event(watchParty, "timer.expired", "#%d %s: %s", t.ID, timerCond(t), t.What))
			continue
		case over:
			f.timedOut, f.reason = true, fmt.Sprintf("timed out at %s, %s did not hold", clock(now, t.Until), timerCond(t))
		default:
			if checked {
				t.Checked, changed = now.UTC(), true
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
