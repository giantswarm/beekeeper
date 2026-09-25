package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
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

func TestAwaitFocus(t *testing.T) {
	log := filepath.Join(t.TempDir(), "main.log")
	focus := func(id string) {
		f, err := os.OpenFile(filepath.Clean(log), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close() //nolint:errcheck // test
		if _, err := fmt.Fprintf(f, "2026-09-25 13:14:04 [info] [CCD] LocalSessions.setFocusedSession: sessionId=%s\n", id); err != nil {
			t.Fatal(err)
		}
	}
	focus("local_prev")
	if awaitFocus(t.Context(), log, "local_new", 300*time.Millisecond) {
		t.Error("awaitFocus while the desktop shows another session: true")
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		focus("null")
		focus("local_new")
	}()
	if !awaitFocus(t.Context(), log, "local_new", 5*time.Second) {
		t.Error("awaitFocus after the import showed the session: false")
	}
}

func TestDesktopStart(t *testing.T) {
	at := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	table := func(args ...[]string) *proc.Table {
		tb := &proc.Table{ByPID: map[int]*proc.Process{}}
		for i, a := range args {
			tb.ByPID[i+1] = &proc.Process{PID: i + 1, Args: a, Start: at}
		}
		return tb
	}
	renderer := []string{"/usr/lib/claude-desktop/claude-desktop --type=renderer --lang=en"}
	for name, tc := range map[string]struct {
		t    *proc.Table
		want time.Time
	}{
		"the argument vector":             {table([]string{"/usr/lib/claude-desktop/claude-desktop", "--ozone-platform=wayland"}), at},
		"a command line Electron rewrote": {table(renderer, []string{"/usr/lib/claude-desktop/claude-desktop --ozone-platform=wayland --password-store=gnome-libsecret"}), at},
		"only its helpers":                {table(renderer, []string{"/usr/lib/claude-desktop/chrome_crashpad_handler"}), time.Time{}},
	} {
		if got := desktopStart(tc.t); !got.Equal(tc.want) {
			t.Errorf("%s: desktopStart = %v, want %v", name, got, tc.want)
		}
	}
}

func TestAwaitReply(t *testing.T) {
	projects := t.TempDir()
	dir := filepath.Join(projects, "-home-x")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "id.jsonl")
	write := func(lines string) {
		if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	running := func() bool { return false }
	write(`{"type":"user","message":{"role":"user","content":"the brief"}}` + "\n")
	if err := awaitReply(t.Context(), projects, "id", running, 0, 700*time.Millisecond); err == nil {
		t.Error("awaitReply before the first reply: no error")
	}
	if err := awaitReply(t.Context(), projects, "id", func() bool { return true }, 0, 5*time.Second); err == nil || !strings.Contains(err.Error(), "ended before its first reply") {
		t.Errorf("awaitReply after the unit ended without a reply = %v", err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		write(`{"type":"user","message":{"role":"user","content":"the brief"}}
{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001"}}
`)
	}()
	start := time.Now()
	if err := awaitReply(t.Context(), projects, "id", running, 500*time.Millisecond, 5*time.Second); err != nil {
		t.Errorf("awaitReply after the first reply = %v", err)
	}
	if took := time.Since(start); took < 600*time.Millisecond {
		t.Errorf("awaitReply returned %s after the start, before the transcript was quiet", took)
	}
}
