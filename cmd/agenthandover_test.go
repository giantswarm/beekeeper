package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	oldID     = "s-old"
	newID     = "s-new"
	countTask = "count the files"
	dueID     = "due"
	countName = "test: count"
)

func TestHandoversDue(t *testing.T) {
	party := func(id string) state.Party { return state.Party{Session: id, Name: "test: " + id} }
	agents := []state.Agent{}
	ids := []string{dueID, "tool", "merging", "small", "said", "sup", "gone"}
	for _, id := range ids {
		agents = append(agents, state.Agent{Party: party(id)})
	}
	st := &state.State{
		Agents:     agents,
		Supervisor: &state.Supervisor{Party: party("sup")},
		Merges:     []state.Merge{{Repo: "o/r", PR: 1, By: party("merging"), Phase: state.Running, PID: 42}},
	}
	var sessions []*claude.Session
	for _, id := range ids[:len(ids)-1] { // "gone" does not run
		sessions = append(sessions, &claude.Session{ID: id, Name: "test: " + id})
	}
	sessions[1].Commands = []claude.Command{{PID: 7, Args: "sleep 60"}}
	contextOf := func(s *claude.Session) int64 {
		if s.ID == "small" {
			return 19_000
		}
		return 25_000
	}
	said := func(p state.Party) bool { return p.Session == ids[4] }
	alive := func(pid int) bool { return pid == 42 }

	due := handoversDue(st, sessions, 20_000, said, contextOf, alive)
	if len(due) != 1 || due[0].agent.Session != dueID || due[0].context != 25_000 {
		t.Fatalf("due = %+v, want only the quiet agent past relayAt", due)
	}
	// The merge ended: the merging agent is quiet now.
	if due := handoversDue(st, sessions, 20_000, said, contextOf, func(int) bool { return false }); len(due) != 2 {
		t.Errorf("after the merge: due = %+v", due)
	}
}

func TestHandoverPromptPassesOnTheBrief(t *testing.T) {
	at := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	h := handover{
		agent:   state.Agent{Party: state.Party{Session: oldID, Name: countName}, Task: countTask},
		context: 25_400,
		record:  &state.Record{Issue: "o/r#61", Waits: "CI"},
		merges:  []state.Merge{{Repo: "o/r", PR: 7, Lane: "main", Phase: state.Waiting}},
		events:  []state.Event{{At: at, Verb: "lease.claim", Detail: "lab-a"}},
		note:    "a and b done; c next",
		brief:   "# Count\n\n## Steps\ncount a, b, c",
	}
	p := h.prompt()
	for _, want := range []string{`You are "` + countName + `"`, "session " + oldID, "at 25k tokens", "Task: " + countTask,
		"Serves: o/r#61, waiting on CI", "a and b done; c next", "o/r#7 in lane main: waiting", "lease.claim lab-a",
		briefOpen + "\n# Count"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if got := h.summary(); got != "task, brief, record, 1 merges, 1 events, note" {
		t.Errorf("summary = %q", got)
	}
	// The next hand-over passes on the brief, not the prompt around it.
	if got := briefOf(p); got != h.brief {
		t.Errorf("briefOf(prompt) = %q", got)
	}
	if got := briefOf("  a plain brief\n"); got != "a plain brief" {
		t.Errorf("briefOf(plain) = %q", got)
	}
	// Without a note and a task, the prompt says so; a huge brief is cut.
	h = handover{agent: state.Agent{Party: state.Party{Session: oldID, Name: "x"}}, brief: strings.Repeat("b", maxBrief)}
	p = h.prompt()
	if !strings.Contains(p, "(none: the brief and the events") || !strings.Contains(p, "report back idle") || len(p) > maxBrief {
		t.Errorf("prompt without note or task: %d bytes", len(p))
	}
}

func TestHandoverStartTakesOverTheRunningEntry(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	old := state.Party{Session: oldID, HostSession: "local_" + oldID, Name: countName}
	st := &state.State{
		Agents:  []state.Agent{{Party: old, Task: countTask, AssignedAt: now.Add(-time.Hour)}},
		Records: []state.Record{{Session: old, Issue: "o/r#61"}},
	}
	running := func(p state.Party) bool { return p.Is(old) }
	s := state.Start{Party: state.Party{Session: newID, HostSession: "local_" + newID, Name: old.Name}, Mode: state.ModeBypass, At: now}
	if _, err := recordStart(st, s, "", running); err == nil {
		t.Fatal("a start under a running session's name must be refused")
	}
	// The hand-over does not count the session it replaces as running.
	handedOver := func(p state.Party) bool { return !p.Is(old) && running(p) }
	reg, err := recordStart(st, s, "", handedOver)
	if err != nil || reg.task != countTask || !reg.assignedAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("registration = %+v, err = %v", reg, err)
	}
	moveRecord(st, old, s.Party)
	if len(st.Agents) != 1 || st.Agents[0].Session != newID || st.Agents[0].Task != countTask {
		t.Errorf("roster = %+v", st.Agents)
	}
	if st.Records[0].Session.Session != newID {
		t.Errorf("record = %+v", st.Records[0])
	}
	// An idle agent's follow-up stays idle.
	st = &state.State{Agents: []state.Agent{{Party: old}}}
	if reg, err := recordStart(st, s, "", handedOver); err != nil || reg.task != "" || !st.Agents[0].AssignedAt.IsZero() {
		t.Errorf("idle: %+v, %v, roster %+v", reg, err, st.Agents)
	}
}

func TestHandoverRefusedWithoutThePermissionHook(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	user := filepath.Join(home, "settings.json")
	dir := filepath.Join(repo, "sub")
	for _, d := range []string{filepath.Join(repo, ".git"), filepath.Join(repo, ".claude"), dir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hook := func(matcher, command string) string {
		return `{"hooks":{"PermissionRequest":[{"matcher":"` + matcher + `","hooks":[{"type":"command","command":"` + command + `"}]}]}}`
	}
	// No settings, another hook, a hook for one tool only: refused.
	if files, ok := permissionHook(user, dir); ok || len(files) != 5 {
		t.Errorf("no settings: ok = %v, files = %v", ok, files)
	}
	write(user, hook("*", "~/bin/other-hook"))
	write(filepath.Join(repo, ".claude", "settings.json"), hook("Bash", "beekeeper hook permissionrequest"))
	if _, ok := permissionHook(user, dir); ok {
		t.Error("another hook or a hook for Bash only must not count")
	}
	// The checkout's local settings carry it.
	write(filepath.Join(repo, ".claude", "settings.local.json"), hook("*", "/bin/sh -c '~/.go/bin/beekeeper --config /s.yaml hook permissionrequest'"))
	if _, ok := permissionHook(user, dir); !ok {
		t.Error("the checkout's settings.local.json has the hook")
	}
	// The user settings carry it.
	write(user, hook("", "~/.go/bin/beekeeper hook permissionrequest"))
	if _, ok := permissionHook(user, t.TempDir()); !ok {
		t.Error("the user settings have the hook")
	}

	// handOver refuses before it asks for the note: nothing is sent,
	// started or stopped.
	a := &app{cfg: &config.Config{Claude: config.Claude{ProjectsDir: filepath.Join(t.TempDir(), "projects")}}, as: "test: supervisor"}
	h := handover{agent: state.Agent{Party: state.Party{Session: oldID, Name: countName}}, dir: t.TempDir()}
	err := a.handOver(t.Context(), h)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != ExitRefused || !strings.Contains(ee.msg, "hook permissionrequest") {
		t.Errorf("handOver without the hook: %v", err)
	}
}
