package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A probe of a host behind the desk's VPN while its login has expired (ssh
// cannot resolve the host), and the same probe once the host answers with
// its condition false.
const (
	sshUnresolved     = `echo "ssh: Could not resolve hostname jump.vpn.example: Name or service not known" >&2; exit 255`
	sshUnresolvedSays = "exit status 255: ssh: Could not resolve hostname jump.vpn.example: Name or service not known"
	carrierDown       = `echo "carrier 0" >&2; exit 1`
	carrierDownSays   = "exit status 1: carrier 0"
)

const probeResumeDeploy = "the link is up: resume the deploy"

// A probe that could not run says nothing of its condition: ssh's own exit
// 255, a host name that did not resolve, a command not found or not
// executable. Only a probe that ran and exits non-zero is "not yet", a
// refused connection included.
func TestProbeThatCannotRunIsNoAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		cmd        string
		unreadable bool
		says       string
	}{
		"ssh cannot resolve its host": {sshUnresolved, true, sshUnresolvedSays},
		"ssh cannot reach its host": {`echo "ssh: connect to host jump port 22: Connection timed out" >&2; exit 255`,
			true, "exit status 255: ssh: connect to host jump port 22: Connection timed out"},
		"a resolver failure before the last line": {`echo "curl: (6) Could not resolve host: api.example" >&2; echo retrying >&2; exit 6`,
			true, "exit status 6: curl: (6) Could not resolve host: api.example"},
		"Go's resolver": {`echo "dial tcp: lookup api.example on 127.0.0.53:53: no such host" >&2; exit 1`,
			true, "exit status 1: dial tcp: lookup api.example on 127.0.0.53:53: no such host"},
		"a command not found":      {"exit 127", true, "exit status 127"},
		"a command not executable": {"exit 126", true, "exit status 126"},
		"a clean false":            {carrierDown, false, carrierDownSays},
		"a refused connection": {`echo "curl: (7) Failed to connect to api.example port 6443: Connection refused" >&2; exit 7`,
			false, "exit status 7: curl: (7) Failed to connect to api.example port 6443: Connection refused"},
	} {
		r := checkOf(state.Timer{Probe: tc.cmd}).result(probeOutput(context.Background(), tc.cmd))
		if r.holds || r.unreadable != tc.unreadable || r.reason != tc.says {
			t.Errorf("%s: %+v", name, r)
		}
	}
}

// A probe that cannot run is one GUIDE PROBLEM naming its error however many
// checks in a row fail so, and timer list says it without -v. The next
// clean check, a false one, is "not yet" and closes the guide's work; the
// probe failing again is a report of its own, and a timer timing out on it
// says it could not run, not that its condition did not hold.
func TestProbeThatCannotRunIsOneGuideProblem(t *testing.T) {
	ctx := context.Background()
	cannot := checkOf(state.Timer{Probe: sshUnresolved}).result(probeOutput(ctx, sshUnresolved))
	clean := checkOf(state.Timer{Probe: carrierDown}).result(probeOutput(ctx, carrierDown))
	st := &state.State{Timers: []state.Timer{{ID: 3284, Due: relayNow, Probe: sshUnresolved, Every: 10 * time.Minute,
		Wake: "app deploy", What: probeResumeDeploy}}}
	var reports []state.Event
	settle := func(n int, r checkResult) []string {
		t.Helper()
		lines, evs, fires, _ := settleTimers(st, map[int]checkResult{3284: r}, relayNow.Add(time.Duration(n)*10*time.Minute))
		if len(fires) != 0 || len(st.Timers) != 1 {
			t.Fatalf("check %d: fires %+v, open %+v", n, fires, st.Timers)
		}
		for _, e := range evs {
			if e.Verb == loop.LoopFileVerb {
				reports = append(reports, e)
			}
		}
		return lines
	}
	lines := settle(0, cannot)
	if len(lines) != 1 || !strings.Contains(lines[0], "#3284, probe `") || !strings.Contains(lines[0], "could not run ("+sshUnresolvedSays+")") {
		t.Fatalf("first failure: %q", lines)
	}
	settle(1, cannot)
	settle(2, cannot)
	if len(reports) != 1 {
		t.Fatalf("%d reports for one run of failed checks: %+v", len(reports), reports)
	}
	guide := &state.State{Guide: &state.Role{Holder: &state.Supervisor{Party: state.Party{Session: guideName}}}}
	problems, _ := guideProblemLines(guide, reports)
	if len(problems) != 1 || !strings.HasPrefix(problems[0], "GUIDE PROBLEM: ") ||
		!strings.Contains(problems[0], "TIMER PROBE FAILED: #3284") || !strings.Contains(problems[0], sshUnresolvedSays) ||
		!strings.Contains(problems[0], "keeps waiting to wake \"app deploy\"") || len(guide.GuideWork) != 1 {
		t.Fatalf("guide feed %q, work %+v", problems, guide.GuideWork)
	}

	w, _, out := notifyingWatch(t, t.TempDir(), false)
	if err := w.store.Update(func(s *state.State) ([]state.Event, error) { s.Timers = st.Timers; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := w.timerList(false); err != nil {
		t.Fatal(err)
	}
	if want := "\n    checked 02:20: could not run since 02:00: " + sshUnresolvedSays + "\n"; !strings.Contains(out.String(), want) {
		t.Fatalf("timer list without -v: %q", out.String())
	}

	if lines := settle(3, clean); len(lines) != 0 {
		t.Fatalf("a clean false said: %q", lines)
	}
	if tm := st.Timers[0]; tm.Unreadable || tm.Reason != carrierDownSays {
		t.Fatalf("after a clean false: %+v", tm)
	}
	if s := lastCheck(relayNow, st.Timers[0]); s != "checked 02:30: not yet: "+carrierDownSays {
		t.Fatalf("list -v after a clean false: %q", s)
	}
	if len(reports) != 2 || !strings.Contains(reports[1].Detail, loop.FileReportSeparator+"RECOVERED: ") {
		t.Fatalf("no recovery: %+v", reports)
	}
	if problems, _ := guideProblemLines(guide, reports); len(problems) != 0 || len(guide.GuideWork) != 0 {
		t.Fatalf("recovery left the guide %q, work %+v", problems, guide.GuideWork)
	}

	settle(4, cannot)
	first, _, _ := loop.Keyed(reports[0].Detail)
	again, _, _ := loop.Keyed(reports[len(reports)-1].Detail)
	if len(reports) != 3 || again == first {
		t.Fatalf("a new run of failed checks: %+v", reports)
	}
	st.Timers[0].Until = relayNow.Add(45 * time.Minute)
	_, _, fires, _ := settleTimers(st, map[int]checkResult{3284: cannot}, relayNow.Add(50*time.Minute))
	if len(fires) != 1 || !strings.Contains(fires[0].reason, "could not run ("+sshUnresolvedSays+")") || strings.Contains(fires[0].reason, "did not hold") {
		t.Fatalf("timed out: %+v", fires)
	}
}

// A probe that holds after checks that could not run fires and closes the
// guide's report. A wake not delivered re-arms the timer from its firing,
// which must not bring the failure back: timer list says nothing of a probe
// that ran.
func TestProbeThatHoldsAfterFailingIsNotListedAsFailing(t *testing.T) {
	cannot := checkOf(state.Timer{Probe: sshUnresolved}).result(probeOutput(context.Background(), sshUnresolved))
	st := &state.State{Timers: []state.Timer{{ID: 3284, Due: relayNow, Probe: sshUnresolved, Every: 10 * time.Minute,
		Wake: "app deploy", What: probeResumeDeploy}}}
	settleTimers(st, map[int]checkResult{3284: cannot}, relayNow)
	if len(st.Timers) != 1 || st.Timers[0].Failing.IsZero() {
		t.Fatalf("a probe that could not run: %+v", st.Timers)
	}
	heldAt := relayNow.Add(10 * time.Minute)
	_, evs, fires, _ := settleTimers(st, map[int]checkResult{3284: {holds: true}}, heldAt)
	recovered := slices.ContainsFunc(evs, func(e state.Event) bool {
		return e.Verb == loop.LoopFileVerb && strings.Contains(e.Detail, loop.FileReportSeparator+"RECOVERED: ")
	})
	if len(fires) != 1 || len(st.Timers) != 0 || !recovered {
		t.Fatalf("held: fires %+v, open %+v, events %+v", fires, st.Timers, evs)
	}
	rearmTimerInState(st, fires[0], "wake not delivered; retrying", heldAt)
	if len(st.Timers) != 1 || !st.Timers[0].Failing.IsZero() {
		t.Fatalf("re-armed after the probe held: %+v", st.Timers)
	}
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	if err := w.store.Update(func(s *state.State) ([]state.Event, error) { s.Timers = st.Timers; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := w.timerList(false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "could not run") {
		t.Fatalf("timer list after the probe held: %q", out.String())
	}
}

// A probe that could not run for a day is stuck without its condition said
// false: it has not been seen to hold, and could not run since when.
func TestProbeThatCannotRunForADayIsNotSaidFalse(t *testing.T) {
	cannot := checkOf(state.Timer{Probe: sshUnresolved}).result(probeOutput(context.Background(), sshUnresolved))
	st := &state.State{Timers: []state.Timer{{ID: 3284, Due: relayNow, Probe: sshUnresolved, What: probeResumeDeploy}}}
	settleTimers(st, map[int]checkResult{3284: cannot}, relayNow)
	day := relayNow.Add(timerStuckAfter)
	lines, _, fires, _ := settleTimers(st, map[int]checkResult{3284: cannot}, day)
	if len(fires) != 0 || len(lines) != 1 || !strings.HasPrefix(lines[0], "TIMER STUCK: #3284, probe `") ||
		!strings.Contains(lines[0], "has not been seen to hold since Sep 25 02:00, could not run since Sep 25 02:00 ("+sshUnresolvedSays+")") ||
		strings.Contains(lines[0], "has not held") {
		t.Fatalf("after a day: %q", lines)
	}
	if s := lastCheck(day, st.Timers[0]); !strings.HasPrefix(s, "not seen to hold since Sep 25 02:00; checked") {
		t.Errorf("list: %q", s)
	}
}
