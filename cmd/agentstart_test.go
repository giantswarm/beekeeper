package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/claude"
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

func TestResumesNamesTheDesktopsCLIOnly(t *testing.T) {
	const id = "25a32485-e71e-462b-8d77-ddbb65220943"
	for args, want := range map[string]bool{
		"claude --output-format stream-json --resume=" + id + " --model default": true,
		"claude --resume " + id: true,
		"claude -p --session-id " + id + " --permission-mode bypassPermissions": false,
		"claude --resume=" + id[:8]:           false,
		"claude --resume /p/" + id + ".jsonl": false,
	} {
		if got := resumes(strings.Fields(args), id); got != want {
			t.Errorf("resumes(%q) = %v, want %v", args, got, want)
		}
	}
}

func TestReopensOnlyAStartTheRosterHolds(t *testing.T) {
	st := &state.State{
		Starts: []state.Start{{Party: state.Party{Session: "on"}}, {Party: state.Party{Session: "handed"}}},
		Agents: []state.Agent{{Party: state.Party{Session: "on", Name: "test: on"}}, {Party: state.Party{Session: "desk", Name: "test: desk"}}},
	}
	for id, want := range map[string]bool{"on": true, "handed": false, "desk": false, "none": false} {
		if got := reopens(st, id); got != want {
			t.Errorf("reopens(%q) = %v, want %v", id, got, want)
		}
	}
}

// A first turn that grew the transcript past the desktop import's window
// leaves the title -n wrote at its start out of reach: titleTranscript puts
// the name in the window's last custom-title line, and the reply's model
// stays readable.
func TestTitleTranscriptReachesTheImportWindow(t *testing.T) {
	projects := t.TempDir()
	dir := filepath.Join(projects, "-home-x")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := titleTranscript(projects, "id", "Guide run 2"); err == nil {
		t.Error("titling a session without a transcript: no error")
	}
	path := filepath.Join(dir, "id.jsonl")
	turn := `{"type":"custom-title","customTitle":"Guide run 2","sessionId":"id"}` + "\n" +
		strings.Repeat(`{"type":"attachment","content":"`+strings.Repeat("x", 1000)+`"}`+"\n", importTitleWindow/1000+10) +
		`{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001"}}` + "\n"
	if err := os.WriteFile(path, []byte(turn), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := titleTranscript(projects, "id", `Guide run 2: Timo's "open" decisions`); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the test's own transcript
	if err != nil {
		t.Fatal(err)
	}
	window := raw[len(raw)-importTitleWindow:]
	lines := strings.Split(strings.TrimSpace(string(window)), "\n")
	var title struct {
		Type        string `json:"type"`
		CustomTitle string `json:"customTitle"`
		SessionID   string `json:"sessionId"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &title); err != nil || title.Type != "custom-title" ||
		title.CustomTitle != `Guide run 2: Timo's "open" decisions` || title.SessionID != "id" {
		t.Errorf("the import window's last line = %q (%v)", lines[len(lines)-1], err)
	}
	if model, err := claude.Model(path); err != nil || model != "claude-haiku-4-5-20251001" {
		t.Errorf("the titled transcript's model = %q, %v", model, err)
	}
}

// The import runs with the first turn's unit frozen and the unit is thawed
// after it, also when the import failed; a unit that ended is not frozen.
func TestWhileFrozenThawsAfterTheImport(t *testing.T) {
	unit := "beekeeper-test-freeze-" + uuid.NewString()[:8]
	if out, err := exec.Command("systemd-run", "--user", "--collect", "--quiet", "--unit="+unit, "--", "sleep", "60").CombinedOutput(); err != nil {
		t.Skipf("no systemd user manager: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("systemctl", "--user", "stop", unit).Run() })
	ctx := context.Background()
	state := func() string {
		out, _ := exec.Command("systemctl", "--user", "show", "-p", "FreezerState", "--value", unit).Output()
		return strings.TrimSpace(string(out))
	}
	boom := errors.New("import failed")
	for _, want := range []error{nil, boom} {
		var during string
		err := whileFrozen(ctx, unit, func() error { during = state(); return want })
		if !errors.Is(err, want) || (want == nil && err != nil) {
			t.Errorf("whileFrozen returned %v, want %v", err, want)
		}
		if during != "frozen" {
			t.Errorf("the unit during the import is %q, want frozen", during)
		}
		if after := state(); after != "running" {
			t.Errorf("the unit after the import (%v) is %q, want running", want, after)
		}
	}
	_ = exec.Command("systemctl", "--user", "stop", unit).Run()
	called := false
	if err := whileFrozen(ctx, unit, func() error { called = true; return nil }); err != nil || !called {
		t.Errorf("an ended unit: whileFrozen = %v, fn called %v", err, called)
	}
}

func TestTitleLine(t *testing.T) {
	for _, c := range []struct{ title, want string }{
		{"Board pull 3", `the desktop titled it "Board pull 3"`},
		{"", `the desktop recorded no title: the sidebar shows it untitled, not as "Board pull 3"`},
		{"General coding session", `the desktop titled it "General coding session", not "Board pull 3"`},
	} {
		if got := titleLine("Board pull 3", c.title); got != c.want {
			t.Errorf("titleLine(%q) = %q, want %q", c.title, got, c.want)
		}
	}
}
