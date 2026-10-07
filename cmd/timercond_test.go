package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The probes that hold and fail, and a condition the tests wait on.
const (
	probeHolds = "true"
	probeFails = "false"
	prMerged7  = "pr-merged o/r#7"
)

func TestTimerConditionsProbeTheirReference(t *testing.T) {
	for when, want := range map[string]string{
		prMerged7:            "gh api repos/o/r/pulls/7 --jq .merged",
		"issue-closed o/r#8": "gh api repos/o/r/issues/8 --jq .state",
		"helmrelease-ready teleport.example.io-mc/flux-giantswarm/backstage": "kubectl --context teleport.example.io-mc -n flux-giantswarm get helmreleases.helm.toolkit.fluxcd.io backstage -o json",
		"controlplane-ready admin@mc/org-x/wc1":                              "kubectl --context admin@mc -n org-x get kubeadmcontrolplanes.controlplane.cluster.x-k8s.io wc1 -o json",
	} {
		c, err := conditionCheck(when)
		if err != nil || c.cmd != want || c.judge == nil {
			t.Errorf("%s: %q, %v", when, c.cmd, err)
		}
	}
	for _, when := range []string{"merged o/r#7", "pr-merged o/r", "pr-merged o/r#7;rm", "helmrelease-ready ctx/ns", "helmrelease-ready c/n/$(id)"} {
		if _, err := conditionCheck(when); err == nil {
			t.Errorf("%q passed", when)
		}
	}
	if c, _ := conditionCheck(prMerged7); !c.github {
		t.Error("pr-merged does not read GitHub")
	}
}

// Ten timers on one reference cost one check per tick, and none until
// their interval has passed.
func TestTimersOnOneReferenceShareOneCheck(t *testing.T) {
	count := filepath.Join(t.TempDir(), "count")
	probe := "echo x >> " + count + "; false"
	var timers []state.Timer
	for i := range 10 {
		timers = append(timers, state.Timer{ID: i + 1, Due: relayNow.Add(-time.Minute), Probe: probe})
	}
	timers = append(timers, state.Timer{ID: 11, Due: relayNow.Add(time.Minute), Probe: probeHolds})
	held := checkTimers(context.Background(), timers, relayNow, false)
	b, _ := os.ReadFile(count) //nolint:gosec // the test's temp file
	if string(b) != "x\n" || len(held) != 10 || held[1].holds || held[1].unreadable {
		t.Fatalf("ran %q, held %v", b, held)
	}
	for i := range timers {
		timers[i].Checked = relayNow
	}
	if held := checkTimers(context.Background(), timers, relayNow.Add(time.Minute), false); len(held) != 0 {
		t.Fatalf("checked again within the interval: %v", held)
	}
	gh := []state.Timer{{ID: 1, Due: relayNow, When: prMerged7}}
	if held := checkTimers(context.Background(), gh, relayNow, true); len(held) != 0 {
		t.Fatalf("read GitHub under the floor: %v", held)
	}
}

// A pr-merged timer wakes its agent within one watch tick of the merge,
// once, and closes.
func TestConditionalTimerWakesItsAgentOnceItHolds(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	merged := false
	fakeRead(t, func(cmd string) ([]byte, error) {
		if !strings.Contains(cmd, "repos/o/r/pulls/7") {
			return nil, errors.New("unexpected " + cmd)
		}
		return []byte(fmt.Sprintln(merged)), nil
	})
	var woke []string
	wasWake := wakeOwner
	wakeOwner = func(_ *app, _ context.Context, _ state.Party, q, msg, _ string) error {
		woke = append(woke, q+": "+msg)
		return nil
	}
	t.Cleanup(func() { wakeOwner = wasWake })
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Session: "s9", Name: "BK 228"}}}
		st.Timers = []state.Timer{{ID: 1, Due: relayNow.Add(-time.Minute), When: prMerged7, Wake: "BK 228", What: "rebase on it"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	live := []*claude.Session{}
	w.pending(context.Background(), live)
	st, _ := w.store.Read()
	if len(woke) != 0 || len(st.Timers) != 1 || !st.Timers[0].Checked.Equal(relayNow.UTC()) {
		t.Fatalf("before the merge: woke %v, timers %+v", woke, st.Timers)
	}
	merged = true
	w.now = relayNow.Add(timerEvery)
	w.pending(context.Background(), live)
	w.pending(context.Background(), live)
	w.timerActs.Wait()
	if len(woke) != 1 || woke[0] != "BK 228: beekeeper timer #1 (pr-merged o/r#7 holds): rebase on it" {
		t.Fatalf("woke %q", woke)
	}
	if st, _ := w.store.Read(); len(st.Timers) != 0 {
		t.Fatalf("still open: %+v", st.Timers)
	}
	if !strings.Contains(out.String(), `TIMER: #1, pr-merged o/r#7 holds: waking "BK 228": rebase on it`) {
		t.Fatalf("watch said %q", out.String())
	}
}

// Past --until a timer fires as timed out, or closes unfired with
// --expire; a timer with only an agent fires at its time; a plain timer is
// left to the watch's TIMER line and timer done.
func TestTimersTimeOutExpireAndFireAtTheirTime(t *testing.T) {
	past := relayNow.Add(-time.Hour)
	st := &state.State{Timers: []state.Timer{
		{ID: 1, Due: past, Probe: probeFails, Until: relayNow, Wake: "a", What: "late"},
		{ID: 2, Due: past, Probe: probeFails, Until: relayNow, Expire: true, What: "moot"},
		{ID: 3, Due: past, What: "plain"},
		{ID: 4, Due: past, Wake: "a", What: "now"},
		{ID: 5, Due: past, Probe: probeFails, Until: relayNow.Add(time.Hour), What: "waits"},
		{ID: 6, Due: relayNow.Add(time.Hour), Wake: "a", What: "later"},
	}}
	lines, evs, fires, changed := settleTimers(st, map[int]checkResult{5: {}}, relayNow)
	if !changed || len(fires) != 2 || !fires[0].timedOut || fires[0].t.ID != 1 || fires[1].t.ID != 4 || fires[1].timedOut {
		t.Fatalf("fires %+v", fires)
	}
	if !strings.HasPrefix(fires[0].message(), "beekeeper timer #1 (timed out at 02:00, probe `false` did not hold): late") {
		t.Errorf("message %q", fires[0].message())
	}
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "TIMER EXPIRED: #2") || len(evs) != 3 || evs[1].Verb != "timer.expired" {
		t.Fatalf("lines %q, events %+v", lines, evs)
	}
	var open []int
	for _, x := range st.Timers {
		open = append(open, x.ID)
	}
	if len(open) != 3 || open[0] != 3 || open[1] != 5 || open[2] != 6 || !st.Timers[1].Checked.Equal(relayNow.UTC()) {
		t.Fatalf("open %v, %+v", open, st.Timers)
	}
	if lines, _ := firePending(st, nil, relayNow); len(lines) != 1 || !strings.HasPrefix(lines[0], "TIMER: #3 due") {
		t.Fatalf("firePending %q", lines)
	}
}

func TestTimerRunLogsItsExitCode(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	w.runTimer(context.Background(), timerFire{t: state.Timer{ID: 4, Run: "echo hi; exit 3", What: "resume"}})
	evs, err := w.store.Events(0, func(e state.Event) bool { return e.Verb == "timer.ran" })
	if err != nil || len(evs) != 1 || !strings.HasPrefix(evs[0].Detail, "#4 exit 3") {
		t.Fatalf("events %+v, %v", evs, err)
	}
	if b, _ := os.ReadFile(filepath.Join(w.store.Dir(), "timers", "4.log")); string(b) != "hi\n" {
		t.Fatalf("log %q", b)
	}
	if !strings.Contains(out.String(), "TIMER RAN: #4 exit 3") {
		t.Fatalf("watch said %q", out.String())
	}
}

func TestTimerAddRefusesWhatCannotFire(t *testing.T) {
	a := &app{now: relayNow}
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	a.store = w.store
	due := relayNow.Add(time.Minute)
	for name, spec := range map[string]timerSpec{
		"until without a condition": {until: "1h"},
		"expire without until":      {probe: probeHolds, expire: true},
		"until before the time":     {probe: probeHolds, until: "30s"},
		"an unknown condition":      {when: "pr-open o/r#1"},
		"an unknown agent":          {wake: "nobody"},
	} {
		if _, err := a.timerFrom(spec, due); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	tm, err := a.timerFrom(timerSpec{when: prMerged7, until: "2h", expire: true}, due)
	if err != nil || tm.When != prMerged7 || !tm.Until.Equal(relayNow.Add(2*time.Hour).UTC()) {
		t.Fatalf("%+v, %v", tm, err)
	}
	if s := timerWhen(relayNow, tm); s != "from 02:01 when pr-merged o/r#7, until 04:00 (then expires)" {
		t.Errorf("when %q", s)
	}
}

func TestRepeatingTimerReArmsAtItsLocalTime(t *testing.T) {
	// relayNow is Friday 2026-09-25 02:00; each timer was due an hour ago.
	past := relayNow.Add(-time.Hour)
	st := &state.State{Timers: []state.Timer{
		{ID: 1, Due: past, Run: probeHolds, Repeat: state.RepeatDaily, What: "overview"},
		{ID: 2, Due: past, Run: probeHolds, Repeat: state.RepeatWeekdays, What: "daily summary"},
		{ID: 3, Due: past, Wake: "a", Repeat: state.RepeatWeekly, What: "weekly summary"},
	}}
	lines, _, fires, changed := settleTimers(st, nil, relayNow)
	if !changed || len(fires) != 3 || len(st.Timers) != 3 {
		t.Fatalf("fires %+v, open %+v", fires, st.Timers)
	}
	for i, want := range []time.Time{
		past.AddDate(0, 0, 1), // Saturday 01:00
		past.AddDate(0, 0, 3), // Monday 01:00, past the weekend
		past.AddDate(0, 0, 7), // next Friday 01:00
	} {
		if !st.Timers[i].Due.Equal(want.UTC()) || !st.Timers[i].Fired.IsZero() {
			t.Errorf("timer #%d due %s, want %s", st.Timers[i].ID, st.Timers[i].Due.Local(), want)
		}
	}
	if !strings.Contains(lines[1], "next Sep 28 01:00") {
		t.Errorf("line %q", lines[1])
	}
}

func TestTimerAddRefusesARepeatThatCannotFire(t *testing.T) {
	a := &app{now: relayNow}
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	a.store = w.store
	due := relayNow.Add(time.Minute)
	for name, spec := range map[string]timerSpec{
		"an unknown repeat":       {run: probeHolds, repeat: "hourly"},
		"a repeat on a condition": {run: probeHolds, repeat: state.RepeatDaily, probe: probeHolds},
		"a repeat on a plain one": {repeat: state.RepeatDaily},
	} {
		if _, err := a.timerFrom(spec, due); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	saturday := relayNow.AddDate(0, 0, 1)
	tm, err := a.timerFrom(timerSpec{run: probeHolds, repeat: state.RepeatWeekdays}, saturday)
	if err != nil || !tm.Due.Equal(relayNow.AddDate(0, 0, 3).UTC()) {
		t.Fatalf("%+v, %v", tm, err)
	}
	if s := timerWhen(relayNow, tm); s != "at Sep 28 02:00, runs `true`, repeats weekdays" {
		t.Errorf("when %q", s)
	}
}
