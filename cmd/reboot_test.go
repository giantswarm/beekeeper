package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestReopenDue(t *testing.T) {
	gone := relayNow
	for _, c := range []struct {
		name          string
		start, liveAt time.Time
		want          bool
	}{
		{"the app does not run", time.Time{}, time.Time{}, false},
		{"the app restarted after the CLI was gone", gone.Add(time.Minute), gone.Add(-time.Hour), true},
		{"the CLI ran under the app and stopped", gone.Add(-time.Hour), gone.Add(-time.Minute), false},
		{"a reboot: the app started before the watch's first poll", gone.Add(-time.Second), time.Time{}, true},
		{"the CLI was last seen before the app started", gone.Add(-time.Minute), gone.Add(-time.Hour), true},
	} {
		if got := reopenDue(c.start, gone, c.liveAt); got != c.want {
			t.Errorf("%s: reopenDue = %v, want %v", c.name, got, c.want)
		}
	}
}

// standbyAfterBoot is a standby watch whose desktop app started at
// appStart, recording the links it opens; the supervisor "Agent four" has
// no spare.
func standbyAfterBoot(t *testing.T, appStart time.Time) (*watcher, *[]string, *bytes.Buffer) {
	t.Helper()
	w, _, out := notifyingWatch(t, t.TempDir(), true)
	var opened []string
	w.spare = spareWatch{
		send:    func(context.Context, string, string) error { return nil },
		open:    func(_ context.Context, url string, _ bool) error { opened = append(opened, url); return nil },
		sent:    map[string]time.Time{},
		checked: map[string]bool{},
	}
	w.table = &proc.Table{ByPID: map[int]*proc.Process{
		7: {PID: 7, Args: []string{"/usr/lib/claude-desktop/claude-desktop", "--ozone-platform=wayland"}, Start: appStart},
		8: {PID: 8, Args: []string{"/usr/lib/claude-desktop/claude-desktop", "--type=renderer"}, Start: appStart},
	}}
	st := &state.State{Supervisor: &state.Supervisor{Party: four, Since: relayNow.Add(-time.Hour)},
		SupervisorCLI: &state.CLI{Supervisor: four, Since: relayNow.Add(-time.Hour), PID: 4242}}
	if err := w.store.Update(func(s *state.State) ([]state.Event, error) { *s = *st; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	return w, &opened, out
}

func TestStandbyReopensTheSupervisorAfterAReboot(t *testing.T) {
	// The login started the app a second before the standby watch's first
	// poll, which is the first to see the supervisor's CLI gone: it stopped
	// with the machine.
	w, opened, out := standbyAfterBoot(t, relayNow.Add(-time.Second))
	for _, at := range []time.Duration{0, 20 * time.Second, 45 * time.Second, 2 * time.Minute} {
		w.now = relayNow.Add(at)
		w.pending(context.Background(), nil)
	}
	if want := []string{"claude://code/continue?session=" + hostFour}; strings.Join(*opened, " ") != want[0] {
		t.Fatalf("opened %q, want %q once:\n%s", *opened, want, out)
	}
	if !strings.Contains(out.String(), `REOPENED: "Agent four"`) {
		t.Errorf("no REOPENED line:\n%s", out)
	}
}

func TestStandbyLeavesACrashUnderTheRunningAppAlone(t *testing.T) {
	// The watch saw the supervisor's CLI run under the app, then it
	// stopped: the app did not restart, no reopen.
	w, opened, out := standbyAfterBoot(t, relayNow.Add(-time.Hour))
	live := []*claude.Session{{ID: "s4", HostID: hostFour, Name: agentFour, PID: 4242}}
	w.now = relayNow
	w.pending(context.Background(), live)
	for _, at := range []time.Duration{10 * time.Second, 45 * time.Second, 2 * time.Minute} {
		w.now = relayNow.Add(at)
		w.pending(context.Background(), nil)
	}
	if len(*opened) != 0 {
		t.Fatalf("reopened after a crash under the running app: %q\n%s", *opened, out)
	}
	if !strings.Contains(out.String(), "SUPERVISOR GONE") {
		t.Errorf("no SUPERVISOR GONE line:\n%s", out)
	}
}

func TestWatchSettlesALostMerge(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	w.now = relayNow
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{
			// Its gate died with the machine.
			{Repo: "o/lost", PR: 1, Lane: "portal", By: four, PID: 0, Phase: state.Running, Started: relayNow.Add(-2 * time.Hour)},
			{Repo: "o/live", PR: 2, Lane: "other", By: four, PID: os.Getpid(), Phase: state.Running, Started: relayNow},
			{Repo: "o/next", PR: 3, Lane: "portal", By: four, Phase: state.Waiting, Joined: relayNow, Seen: relayNow},
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w.lostMerges()
	w.lostMerges()
	if n := strings.Count(out.String(), "MERGE LOST: o/lost#1 in lane portal"); n != 1 {
		t.Fatalf("MERGE LOST said %d times:\n%s", n, out)
	}
	st, err := w.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range st.Merges {
		want := map[string]string{"o/lost": state.Settling, "o/live": state.Running, "o/next": state.Waiting}[m.Repo]
		if m.Phase != want {
			t.Errorf("%s is %s, want %s", m.Key(), m.Phase, want)
		}
	}
	evs, err := w.store.Events(10, func(e state.Event) bool { return e.Verb == "merge.lost" })
	if err != nil || len(evs) != 1 {
		t.Errorf("merge.lost events: %d, %v", len(evs), err)
	}
}

func TestStoppedAgentsAreListedOnce(t *testing.T) {
	bg := state.Party{Session: "bg-1", Name: "worker one"}
	agents := []state.Agent{
		{Party: bg, Task: "fix beekeeper#1"},
		{Party: four, Task: "fix beekeeper#2"},
		{Party: state.Party{Session: "s5", Name: "idle one"}},
	}
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	st := &state.State{Agents: agents}
	w.stoppedAgents(st, nil)
	w.stoppedAgents(st, nil)
	l := out.String()
	if strings.Count(l, "AGENTS STOPPED") != 1 || !strings.Contains(l, `"worker one" (claude --bg --resume bg-1 "`) ||
		!strings.Contains(l, `"Agent four" (open claude://code/continue?session=local_4)`) || strings.Contains(l, "idle one") {
		t.Fatalf("stopped agents:\n%s", l)
	}
	// Resumed, then stopped again: said again.
	w.stoppedAgents(st, []*claude.Session{{ID: "bg-1", Name: "worker one"}})
	out.Reset()
	w.stoppedAgents(st, nil)
	if l := out.String(); !strings.Contains(l, "worker one") || strings.Contains(l, "Agent four") {
		t.Errorf("a stop after the resume:\n%s", l)
	}

	var prompt bytes.Buffer
	a := &app{out: &prompt, now: relayNow}
	a.promptAgents(func(f string, args ...any) { prompt.WriteString(strings.TrimSpace(fmt.Sprintf(f, args...)) + "\n") }, agents, nil)
	if p := prompt.String(); !strings.Contains(p, `"worker one" on fix beekeeper#1 since`) ||
		!strings.Contains(p, "its CLI is not running (stopped by a reboot or a crash): resume it with `claude --bg --resume bg-1") ||
		!strings.Contains(p, `"idle one" idle since`) {
		t.Errorf("handover prompt:\n%s", p)
	}
}
