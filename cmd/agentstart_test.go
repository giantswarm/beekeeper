package cmd

import (
	"slices"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestRecordStart(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	live := func(p state.Party) bool { return p.Session == "r" }
	start := func(id, name string, at time.Time) state.Start {
		return state.Start{Party: state.Party{Session: id, HostSession: "local_" + id, Name: name}, Mode: state.ModeBypass, Dir: "/w", At: at}
	}
	old := start("old", "test: gone", now.Add(-startsKept-time.Hour))
	kept := start("kept", "test: kept", now.Add(-startsKept+time.Hour))

	// A start is recorded with its mode, the old ones forgotten, and the
	// agent registered busy with the brief.
	st := &state.State{Starts: []state.Start{old, kept}}
	reg, err := recordStart(st, start("n", workerName, now), "Brief: count lines", live)
	if err != nil || reg.task != "Brief: count lines" {
		t.Fatalf("registration = %+v, err = %v", reg, err)
	}
	if ids := []string{st.Starts[0].Session, st.Starts[len(st.Starts)-1].Session}; len(st.Starts) != 2 || !slices.Equal(ids, []string{"kept", "n"}) {
		t.Errorf("starts = %+v", st.Starts)
	}
	if s, ok := st.BypassStart("n"); !ok || s.Name != workerName {
		t.Errorf("BypassStart(n) = %+v, %v", s, ok)
	}
	if ag := st.Agents[0]; len(st.Agents) != 1 || ag.Session != "n" || ag.HostSession != "local_n" || ag.Task != "Brief: count lines" || !ag.AssignedAt.Equal(now) {
		t.Errorf("roster = %+v", st.Agents)
	}

	// A start under the name of a stopped session takes over its open
	// task; a running session's name is refused and nothing is recorded.
	st = &state.State{Agents: []state.Agent{
		{Party: state.Party{Session: "a", Name: workerName}, Task: staleTask, AssignedAt: now.Add(-time.Hour)},
		{Party: state.Party{Session: "r", Name: "test: busy"}, Task: "x"},
	}}
	if reg, err := recordStart(st, start("m", workerName, now), "Brief: other", live); err != nil || reg.task != staleTask || st.Agents[1].Task != staleTask {
		t.Errorf("take over: %+v, %v, roster %+v", reg, err, st.Agents)
	}
	if _, err := recordStart(st, start("z", "test: busy", now), "Brief", live); err == nil || len(st.Starts) != 1 {
		t.Errorf("a running session's name: err = %v, starts = %+v", err, st.Starts)
	}
}

func TestBypassStartNeedsTheRecordedMode(t *testing.T) {
	st := &state.State{Starts: []state.Start{{Party: state.Party{Session: "d"}, Mode: "default"}, {Party: state.Party{Session: "b"}, Mode: state.ModeBypass}}}
	for id, want := range map[string]bool{"b": true, "d": false, "x": false, "": false} {
		if _, ok := st.BypassStart(id); ok != want {
			t.Errorf("BypassStart(%q) = %v", id, ok)
		}
	}
}

func TestAgentArgvAndBriefTask(t *testing.T) {
	got := agentArgv("/usr/bin/claude", "id-1", "test: w", "haiku", "-starts with a dash")
	want := []string{"/usr/bin/claude", "-p", "--session-id", "id-1", "--permission-mode", "bypassPermissions", "-n", "test: w", "--model", "haiku", "--", "-starts with a dash"}
	if !slices.Equal(got, want) {
		t.Errorf("argv = %q", got)
	}
	if got := agentArgv("claude", "id", "n", "", "b"); slices.Contains(got, "--model") {
		t.Errorf("argv without a model = %q", got)
	}
	if got := briefTask("# Brief: bk-permhook\n\nbody"); got != "Brief: bk-permhook" {
		t.Errorf("briefTask = %q", got)
	}
}
