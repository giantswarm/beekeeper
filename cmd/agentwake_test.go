package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A wake resumes the CLI session id the desktop record names now, in its
// directory; beekeeper's start keeps its bypass, a desktop session its
// recorded mode, and a session known only by its transcript its cwd.
func TestResolveWake(t *testing.T) {
	const started, name = "st-1", "test: waker"
	root := t.TempDir()
	cfg := &config.Config{Claude: config.Claude{ProjectsDir: filepath.Join(root, "projects"), DesktopDir: filepath.Join(root, "desk")}}
	writeFile(t, filepath.Join(cfg.Claude.ProjectsDir, "-w", started+".jsonl"), `{"type":"user"}`+"\n")
	writeFile(t, filepath.Join(cfg.Claude.ProjectsDir, "-w", "cli-2.jsonl"), `{"type":"user"}`+"\n")
	writeFile(t, filepath.Join(cfg.Claude.ProjectsDir, "-bg", "bg.jsonl"), `{"type":"summary"}`+"\n"+`{"type":"user","cwd":"/bg"}`+"\n")
	writeFile(t, filepath.Join(cfg.Claude.DesktopDir, "a", "b", "local_"+started+".json"),
		`{"sessionId":"local_st-1","cliSessionId":"st-1","cwd":"/w","permissionMode":"acceptEdits","title":"test: waker"}`)
	writeFile(t, filepath.Join(cfg.Claude.DesktopDir, "a", "b", "local_desk.json"),
		`{"sessionId":"local_desk","cliSessionId":"cli-2","cwd":"/d","permissionMode":"bypassPermissions","title":"Supervisor","model":"claude-opus-5-5"}`)
	st := &state.State{Starts: []state.Start{{Party: state.Party{Session: started}, Mode: state.ModeBypass, Dir: "/w"}}}

	for _, tc := range []struct {
		ag   state.Agent
		want wakeTarget
	}{
		{state.Agent{Party: state.Party{Session: started, HostSession: "local_" + started, Name: name}},
			wakeTarget{name: name, id: started, host: "local_" + started, dir: "/w", mode: state.ModeBypass}},
		{state.Agent{Party: state.Party{Session: "cli-1", HostSession: "local_desk", Name: "Supervisor"}},
			wakeTarget{name: "Supervisor", id: "cli-2", host: "local_desk", dir: "/d", mode: state.ModeBypass, model: "claude-opus-5-5"}},
		{state.Agent{Party: state.Party{Session: "bg", Name: "bg worker"}},
			wakeTarget{name: "bg worker", id: "bg", dir: "/bg", mode: "acceptEdits"}},
	} {
		got, err := resolveWake(cfg, st, tc.ag)
		if err != nil || got != tc.want {
			t.Errorf("resolveWake(%s) = %+v, %v, want %+v", tc.ag.Name, got, err, tc.want)
		}
	}
	if _, err := resolveWake(cfg, st, state.Agent{Party: state.Party{Session: "vanished", Name: "vanished"}}); err == nil || !strings.Contains(err.Error(), "no transcript") {
		t.Errorf("a session without a transcript: %v", err)
	}
}

func TestWakeArgv(t *testing.T) {
	const name, model, id = "test: waker", "sonnet", "w-1"
	got := wakeArgv("/usr/bin/claude", wakeTarget{name: name, id: id, mode: state.ModeBypass, model: model}, "-a message with a dash")
	want := []string{"/usr/bin/claude", "-p", resumeFlag, id, "--permission-mode", state.ModeBypass, "-n", name, "--model", model, "--", "-a message with a dash"}
	if !slices.Equal(got, want) {
		t.Errorf("wakeArgv = %q, want %q", got, want)
	}
	if u := wakeUnit("0123456789ab"); u != "beekeeper-wake-01234567" {
		t.Errorf("wakeUnit = %q", u)
	}
}

// Only a headless claude of the session is a beekeeper turn: the desktop's
// CLI resumes it too, without -p.
func TestHeadlessTurn(t *testing.T) {
	tb := &proc.Table{ByPID: map[int]*proc.Process{
		1: {PID: 1, Comm: claudeComm, Args: []string{claudeComm, "-p", sessionIDFlag, "first", "--", "brief"}},
		2: {PID: 2, Comm: claudeComm, Args: []string{claudeComm, "-p", resumeFlag, "woken", "--", "msg"}},
		3: {PID: 3, Comm: claudeComm, Args: []string{claudeComm, "--input-format", "stream-json", "--resume=on-screen"}},
	}}
	for id, want := range map[string]string{"first": "first turn", "woken": "wake turn", "on-screen": "", "none": "", "": ""} {
		if got := headlessTurn(tb, id); got != want {
			t.Errorf("headlessTurn(%q) = %q, want %q", id, got, want)
		}
	}
	if headlessTurn(nil, "first") != "" {
		t.Error("no table: no turn")
	}
}

// A send by name needs one running CLI under the name: a headless turn and a
// desktop CLI beside it are two.
func TestUniqueNameAndWakeLive(t *testing.T) {
	const name, otherHost = "gpm worker", "local_o"
	head := &claude.Session{PID: 10, ID: "s1", Name: name}
	desk := &claude.Session{PID: 11, ID: "s1", HostID: "local_s1", Name: name}
	other := &claude.Session{PID: 12, ID: "o", HostID: otherHost, Name: "someone"}
	if n, err := uniqueName([]*claude.Session{head, other}, head); err != nil || n != name {
		t.Errorf("one CLI: %q, %v", n, err)
	}
	if _, err := uniqueName([]*claude.Session{head, desk, other}, head); err == nil || !strings.Contains(err.Error(), "10, 11") && !strings.Contains(err.Error(), "11, 10") {
		t.Errorf("two CLIs under one name: %v", err)
	}
	sessions := []*claude.Session{head, other}
	if s, ok := wakeLive(sessions, state.Party{HostSession: "local_s1"}, "s1"); !ok || s != head {
		t.Errorf("a headless turn is found by its session id: %v %v", s, ok)
	}
	if s, ok := wakeLive(sessions, state.Party{HostSession: otherHost}, "x"); !ok || s != other {
		t.Errorf("a desktop CLI is found by its host id: %v %v", s, ok)
	}
	if _, ok := wakeLive(sessions, state.Party{HostSession: "local_s3"}, "s3"); ok {
		t.Error("a stopped session has no CLI")
	}
}
