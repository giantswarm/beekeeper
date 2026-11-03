package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	prChecks7   = "pr-checks o/r#7"
	ciHead      = "abcdef0123456789abcdef0123456789abcdef01"
	ciLintURL   = "https://github.com/o/r/actions/runs/9/job/90"
	ciRunning   = `{"status":"IN_PROGRESS","conclusion":"","name":"go-build"}`
	ciSucceeded = `{"status":"COMPLETED","conclusion":"SUCCESS","name":"go-build"}`
	ciLintRed   = `{"status":"COMPLETED","conclusion":"FAILURE","name":"pre-commit","detailsUrl":"` + ciLintURL + `"}`
	ciStatusOK  = `{"state":"SUCCESS","context":"ci/circleci: setup"}`
)

// ciAnswer is gh pr view's answer for a pull request in state with the
// rollup's entries.
func ciAnswer(state string, rollup ...string) []byte {
	return []byte(`{"state":"` + state + `","headRefOid":"` + ciHead + `","statusCheckRollup":[` + strings.Join(rollup, ",") + `]}`)
}

func TestPRChecksConditionReadsTheRollup(t *testing.T) {
	c, err := conditionCheck(prChecks7)
	if err != nil || c.cmd != "gh pr view 7 --repo o/r --json state,headRefOid,statusCheckRollup" || !c.github || c.judge == nil {
		t.Fatalf("%+v, %v", c, err)
	}
	for _, bad := range []string{"pr-checks o/r", "pr-checks o/r#7;rm"} {
		if _, err := conditionCheck(bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
	for name, tc := range map[string]struct {
		out    []byte
		holds  bool
		reason string
	}{
		"no check yet":   {ciAnswer("OPEN"), false, "no check on head abcdef012 yet"},
		"still running":  {ciAnswer("OPEN", ciRunning, ciStatusOK), false, "1 of 2 checks on head abcdef012 still running"},
		"all green":      {ciAnswer("OPEN", ciSucceeded, ciStatusOK), true, "CI green on head abcdef012: every check passed"},
		"red, one runs":  {ciAnswer("OPEN", ciRunning, ciLintRed), true, "CI red on head abcdef012: pre-commit " + ciLintURL},
		"cancelled":      {ciAnswer("OPEN", `{"status":"COMPLETED","conclusion":"CANCELLED","name":"e2e"}`), true, "CI red on head abcdef012: e2e"},
		"timed out":      {ciAnswer("OPEN", `{"status":"COMPLETED","conclusion":"TIMED_OUT","name":"slow","detailsUrl":"u"}`), true, "CI red on head abcdef012: slow u"},
		"status error":   {ciAnswer("OPEN", `{"state":"ERROR","context":"ci/x","targetUrl":"t"}`), true, "CI red on head abcdef012: ci/x t"},
		"pending status": {ciAnswer("OPEN", `{"state":"PENDING","context":"ci/x"}`), false, "1 of 1 checks on head abcdef012 still running"},
		"pr merged":      {ciAnswer("MERGED", ciRunning), true, "o/r#7 is merged at head abcdef012"},
		"closed":         {ciAnswer("CLOSED"), true, "o/r#7 is closed at head abcdef012"},
	} {
		r := c.result(tc.out, nil)
		if r.unreadable || r.holds != tc.holds || r.reason != tc.reason {
			t.Errorf("%s: %+v, want holds %v reason %q", name, r, tc.holds, tc.reason)
		}
	}
	if r := c.result([]byte("not json"), nil); !r.unreadable {
		t.Errorf("garbage read as %+v", r)
	}
	if r := c.result([]byte(`{}`), nil); !r.unreadable {
		t.Errorf("an empty answer read as %+v", r)
	}
	if r := c.result(nil, errors.New("exit status 1: no pull request")); !r.unreadable {
		t.Errorf("a failed read read as %+v", r)
	}
}

// The workflow of a worker that parks on its pull request's CI as a plain
// wait, with a pr-checks timer: the timer keeps waiting while the checks
// run, wakes the worker once a check fails, naming it and its URL, and
// closes, so the same failure wakes it no second time. A green run wakes it
// with that outcome.
func TestPlainCIParkWakesOnRedAndOnGreen(t *testing.T) {
	for name, tc := range map[string]struct {
		settled []byte
		says    string
	}{
		"red":   {ciAnswer("OPEN", ciRunning, ciLintRed), "pr-checks o/r#7 holds: CI red on head abcdef012: pre-commit " + ciLintURL},
		"green": {ciAnswer("OPEN", ciSucceeded, ciStatusOK), "pr-checks o/r#7 holds: CI green on head abcdef012: every check passed"},
	} {
		t.Run(name, func(t *testing.T) {
			w, _, out := notifyingWatch(t, t.TempDir(), false)
			w.chores = true
			answer := ciAnswer("OPEN", ciRunning, ciStatusOK)
			reads := 0
			fakeRead(t, func(cmd string) ([]byte, error) {
				if !strings.Contains(cmd, "gh pr view 7 --repo o/r") {
					return nil, errors.New("unexpected " + cmd)
				}
				reads++
				return answer, nil
			})
			var woke []string
			wasWake := wakeOwner
			wakeOwner = func(_ *app, _ context.Context, _ state.Party, q, msg, _ string) error {
				woke = append(woke, q+": "+msg)
				return nil
			}
			t.Cleanup(func() { wakeOwner = wasWake })
			if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents = []state.Agent{{Party: state.Party{Session: "s9", Name: bk228}, Task: "fix o/r#7",
					Park: &state.Park{On: claimIssue, At: relayNow.Add(-time.Minute), Waits: "CI"}}}
				st.Timers = []state.Timer{{ID: 1, Due: relayNow.Add(-time.Minute), When: prChecks7, Wake: bk228, What: "CI settled: fix or park for review", Until: relayNow.Add(2 * time.Hour)}}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			live := []*claude.Session{}
			w.pending(context.Background(), live)
			st, _ := w.store.Read()
			if len(woke) != 0 || len(st.Timers) != 1 || st.Timers[0].Reason != "1 of 2 checks on head abcdef012 still running" {
				t.Fatalf("while running: woke %v, timers %+v", woke, st.Timers)
			}
			answer = tc.settled
			w.now = relayNow.Add(timerEvery)
			w.pending(context.Background(), live)
			w.timerActs.Wait()
			want := "BK 228: beekeeper timer #1 (" + tc.says + "): CI settled: fix or park for review"
			if len(woke) != 1 || woke[0] != want {
				t.Fatalf("woke %q, want %q", woke, want)
			}
			if !strings.Contains(out.String(), "TIMER: #1, "+tc.says) {
				t.Errorf("watch said %q", out.String())
			}
			// Closed: later ticks neither read nor wake again.
			before := reads
			w.now = relayNow.Add(3 * timerEvery)
			w.pending(context.Background(), live)
			w.timerActs.Wait()
			if st, _ := w.store.Read(); len(st.Timers) != 0 || len(woke) != 1 || reads != before {
				t.Fatalf("after firing: timers %+v, woke %v, reads %d→%d", st.Timers, woke, before, reads)
			}
		})
	}
}
