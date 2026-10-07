package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The watch says each agent whose shell resolves a gh other than the agent's
// own (agents.shell.path) once, and its end once the agent's gh is its own.
func TestTheWatchSaysAnUnbrokeredGH(t *testing.T) {
	const own, plain, missing, unread = "Own", "Plain", "Missing", "Unread"
	dir := t.TempDir()
	link := filepath.Join(dir, "agent-bin")
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("stateDir: "+dir+"\nagents: {shell: {path: ["+link+"]}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	set := func(gh map[string]string) {
		t.Helper()
		if err := store.Update(func(st *state.State) ([]state.Event, error) {
			st.Agents = nil
			for _, name := range []string{own, plain, missing, unread} {
				st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: name, Session: name}, GH: gh[name], GHAt: now})
			}
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	line := func(name, gh string) string { return fmt.Sprintf("GH UNBROKERED %q: its shell resolves gh to %s", name, gh) }
	set(map[string]string{own: filepath.Join(link, "gh"), plain: "/usr/bin/gh", missing: ghNone})
	var out bytes.Buffer
	w := &watcher{app: &app{cfg: cfg, store: store, now: now, out: &out}, last: map[string]time.Time{}}
	w.unbrokered()
	w.unbrokered()
	s := out.String()
	if strings.Count(s, line(plain, "/usr/bin/gh")) != 1 || strings.Count(s, line(missing, ghNone)) != 1 ||
		strings.Contains(s, `"`+own+`"`) || strings.Contains(s, `"`+unread+`"`) {
		t.Errorf("watch:\n%s", s)
	}

	out.Reset()
	set(map[string]string{own: filepath.Join(link, "gh"), plain: filepath.Join(link, "gh"), missing: ghNone})
	w.unbrokered()
	if s := out.String(); !strings.Contains(s, fmt.Sprintf("ENDED GH UNBROKERED %q", plain)) || strings.Contains(s, fmt.Sprintf("ENDED GH UNBROKERED %q", missing)) {
		t.Errorf("watch after %s's gh became its own:\n%s", plain, s)
	}
}

// recordGH records on the session's own roster entry only.
func TestRecordGHOnTheRoster(t *testing.T) {
	now := time.Now()
	st := &state.State{Agents: []state.Agent{{Party: state.Party{Name: "A", Session: "s1"}}}}
	if err := recordGH(st, state.Party{Session: "s2"}, "/usr/bin/gh", now); !errors.Is(err, errNotAgent) {
		t.Errorf("a session off the roster: %v, want errNotAgent", err)
	}
	if err := recordGH(st, state.Party{Session: "s1"}, "", now); err != nil || st.Agents[0].GH != ghNone || !st.Agents[0].GHAt.Equal(now) {
		t.Errorf("no gh: %v, recorded %q at %s", err, st.Agents[0].GH, st.Agents[0].GHAt)
	}
}

// Each session start of an agent on the roster records the gh its shell
// resolves once the prelude ran: the agent's own with agents.shell.path
// first, the person's without it.
func TestSessionStartRecordsTheAgentsGH(t *testing.T) {
	dir := t.TempDir()
	own, plain := filepath.Join(dir, "agent-bin"), filepath.Join(dir, "usr-bin")
	for _, d := range []string{own, plain} {
		writeFile(t, filepath.Join(d, "gh"), "#!/bin/sh\n")
		if err := os.Chmod(filepath.Join(d, "gh"), 0o700); err != nil { //nolint:gosec // a fake gh
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	start := func(conf string) string {
		t.Helper()
		if err := os.WriteFile(cfgPath, []byte("stateDir: "+dir+"\n"+conf), 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		runHook(t, &app{cfgPath: cfgPath, out: &out}, &out, "sessionstart", `{"hook_event_name":"SessionStart","session_id":"s1","cwd":"`+dir+`"}`)
		store, err := state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.Read()
		if err != nil {
			t.Fatal(err)
		}
		return st.Agents[0].GH
	}
	t.Setenv("CLAUDE_ENV_FILE", filepath.Join(dir, "env"))
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
	t.Setenv("BEEKEEPER_SANDBOX", "")
	t.Setenv("PATH", plain)
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Name: "A", Session: "s1"}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if gh := start("agents: {shell: {path: [" + own + "]}}\n"); gh != filepath.Join(own, "gh") {
		t.Errorf("with agents.shell.path: recorded %q", gh)
	}
	if gh := start(""); gh != filepath.Join(plain, "gh") {
		t.Errorf("without agents.shell.path: recorded %q", gh)
	}
}
