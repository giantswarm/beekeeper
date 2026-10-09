package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/peer"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	oldID     = "s-old"
	newID     = "s-new"
	countTask = "count the files"
	dueID     = "due"
	countName = "test: count"
	issue61   = "o/r#61"
	noteAB    = "a and b done; c next"
)

func TestHandoversDue(t *testing.T) {
	party := func(id string) state.Party { return state.Party{Session: id, Name: "test: " + id} }
	agents := []state.Agent{}
	ids := []string{dueID, "tool", "merging", "small", "said", "sup", "relieved", "gone", "parked", "parked-small"}
	for _, id := range ids {
		agents = append(agents, state.Agent{Party: party(id)})
	}
	// Two agents with a task whose headless turn ended: their CLI does not
	// run.
	agents[8].Task, agents[9].Task = countTask, countTask
	st := &state.State{
		Agents:     agents,
		Supervisor: &state.Supervisor{Party: party("sup")},
		Relieved:   []state.Relief{{Party: party("relieved")}},
		Merges:     []state.Merge{{Repo: scratchRepo, PR: 1, By: party("merging"), Phase: state.Running, PID: 42}},
	}
	var sessions []*claude.Session
	for _, id := range ids[:7] { // "gone" and the parked ones do not run
		sessions = append(sessions, &claude.Session{ID: id, Name: "test: " + id})
	}
	sessions[1].Commands = []claude.Command{{PID: 7, Args: "sleep 60"}}
	contextOf := func(ag state.Agent, s *claude.Session) int64 {
		if (s == nil) != strings.HasPrefix(ag.Session, "parked") && ag.Session != ids[7] {
			t.Errorf("%s: session %v", ag.Session, s)
		}
		if strings.HasSuffix(ag.Session, "small") {
			return 19_000
		}
		return 25_000
	}
	said := func(p state.Party) bool { return p.Session == ids[4] }
	alive := func(pid int) bool { return pid == 42 }
	cfg := config.Agents{RelayAt: 20_000, LastStepGrace: config.Duration{Duration: 30 * time.Minute}, LastStepCeiling: 30_000}
	now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

	due := handoversDue(st, sessions, cfg, now, said, contextOf, alive)
	if len(due) != 2 || due[0].agent.Session != dueID || due[0].context != 25_000 || due[0].parked || due[0].held != "" || due[0].was != "" {
		t.Fatalf("due = %+v, want the quiet agent past relayAt first", due)
	}
	// The agent whose turn ended with its task open is due too; the one
	// without a task ("gone") is not.
	if due[1].agent.Session != "parked" || !due[1].parked {
		t.Errorf("due = %+v, want the parked agent past relayAt second", due)
	}
	// The merge ended: the merging agent is quiet now.
	if due := handoversDue(st, sessions, cfg, now, said, contextOf, func(int) bool { return false }); len(due) != 3 {
		t.Errorf("after the merge: due = %+v", due)
	}
}

// An agent over relayAt whose task is in its last step is held, not due: its
// serve record waits on the report or says the merge landed, or the gate saw
// its merge land. The hold lasts lastStepGrace from that evidence and ends at
// lastStepCeiling; an agent that reported done is never handed over.
func TestHandoversHeldInTheLastStep(t *testing.T) {
	now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	party := func(id string) state.Party { return state.Party{Session: id, Name: "test: " + id} }
	const finished, late, closed = "reported-done", "stale-record", "ended-record"
	ids := []string{"report", "merged", "landed", "ci", finished, late, closed}
	st := &state.State{}
	var sessions []*claude.Session
	for _, id := range ids {
		st.Agents = append(st.Agents, state.Agent{Party: party(id), Task: countTask})
		sessions = append(sessions, &claude.Session{ID: id, Name: "test: " + id})
	}
	st.Agents[4].Done, st.Agents[4].Task = true, ""
	rec := func(id, waits string, ago time.Duration) state.Record {
		return state.Record{Session: party(id), Issue: issue61, Waits: waits, At: now.Add(-ago)}
	}
	st.Records = []state.Record{
		rec("report", "writing the report", 10*time.Minute),
		rec("merged", "PR 61 merged, the proof runs", 5*time.Minute),
		rec("ci", "CI of PR 61", time.Minute),
		rec(late, "the report", time.Hour),
		rec(closed, "the report", time.Minute),
	}
	st.Records[4].Ended = now
	st.Merges = []state.Merge{
		{Repo: scratchRepo, PR: 61, By: party("landed"), Phase: state.Settling, Finished: now.Add(-2 * time.Minute)},
		{Repo: scratchRepo, PR: 62, By: party("ci"), Phase: state.Settling, Finished: now.Add(-time.Minute), Exit: -1}, // lost: whether it merged is unknown
	}
	cfg := config.Agents{RelayAt: 20_000, LastStepGrace: config.Duration{Duration: 30 * time.Minute}, LastStepCeiling: 30_000}
	contextOf := func(state.Agent, *claude.Session) int64 { return 25_000 }
	none := func(state.Party) bool { return false }
	find := func(due []dueAgent) map[string]dueAgent {
		got := map[string]dueAgent{}
		for _, d := range due {
			got[d.agent.Session] = d
		}
		return got
	}

	got := find(handoversDue(st, sessions, cfg, now, none, contextOf, func(int) bool { return false }))
	if _, done := got[finished]; len(got) != 6 || done {
		t.Fatalf("due = %+v, want every agent but the done one", got)
	}
	held := map[string]string{
		"report": `it waits on "writing the report"`,
		"merged": `it waits on "PR 61 merged, the proof runs"`,
		"landed": scratchRepo + "#61 merged",
	}
	for id, why := range held {
		if d := got[id]; d.held != why || d.was != "" {
			t.Errorf("%s: held %q, was %q; want held %q", id, d.held, d.was, why)
		}
	}
	if d := got["report"]; !d.until.Equal(now.Add(20 * time.Minute)) {
		t.Errorf("report: held until %s, want 20m from now (30m from its record)", d.until)
	}
	for _, id := range []string{"ci", closed} {
		if d := got[id]; d.held != "" || d.was != "" {
			t.Errorf("%s: held %q, was %q; want due as before", id, d.held, d.was)
		}
	}
	if d, want := got[late], `held 30m for its last step (it waits on "the report")`; d.held != "" || d.was != want {
		t.Errorf("%s: held %q, was %q; want due, %s", late, d.held, d.was, want)
	}

	// At the ceiling the last step holds nothing.
	cfg.LastStepCeiling = 25_000
	got = find(handoversDue(st, sessions, cfg, now, none, contextOf, func(int) bool { return false }))
	for id := range held {
		if d := got[id]; d.held != "" || !strings.HasPrefix(d.was, "its last step (") || !strings.HasSuffix(d.was, ") holds no hand-over past 25k") {
			t.Errorf("%s at the ceiling: held %q, was %q", id, d.held, d.was)
		}
	}
}

// The watch says HANDOVER HELD once for an agent in its last step and, once
// the grace is over, HANDOVER DUE once, saying how long it was held.
func TestWatchSaysHandoverHeldOnce(t *testing.T) {
	dir := t.TempDir()
	w, _, out := notifyingWatch(t, dir, false)
	w.notifier = nil
	ts := w.now.Add(-time.Minute).UTC().Format(time.RFC3339)
	tr := filepath.Join(dir, "s1.jsonl")
	line := `{"type":"assistant","timestamp":"` + ts + `","message":{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":10,"cache_read_input_tokens":410000},"content":[]}}` + "\n"
	if err := os.WriteFile(tr, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	one := state.Party{Session: "s1", Name: sessionOne}
	st := &state.State{
		Agents:  []state.Agent{{Party: one, Task: countTask}},
		Records: []state.Record{{Session: one, Issue: issue61, Waits: "the report", At: w.now.Add(-time.Minute)}},
	}
	live := []*claude.Session{{ID: "s1", Name: sessionOne, Transcript: tr, LastActive: w.now}}

	w.handoversDue(st, live)
	w.handoversDue(st, live)
	held := fmt.Sprintf(`HANDOVER HELD %q at 410k: last step, report expected (it waits on "the report"); due at %s or at 500k`,
		sessionOne, w.now.Add(29*time.Minute).Local().Format("15:04"))
	if got := out.String(); strings.Count(got, held) != 1 || strings.Contains(got, "HANDOVER DUE") {
		t.Errorf("want one held line %q, got:\n%s", held, got)
	}

	out.Reset()
	w.now = w.now.Add(time.Hour)
	w.handoversDue(st, live)
	w.handoversDue(st, live)
	due := fmt.Sprintf(`HANDOVER DUE %q at 410k, held 30m for its last step (it waits on "the report"): beekeeper agents handover %q`, sessionOne, sessionOne)
	if got := out.String(); strings.Count(got, due) != 1 || strings.Contains(got, "HANDOVER HELD") {
		t.Errorf("want one due line %q, got:\n%s", due, got)
	}
}

func TestHandoverPromptPassesOnTheBrief(t *testing.T) {
	at := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	h := handover{
		agent:   state.Agent{Party: state.Party{Session: oldID, Name: countName}, Task: countTask},
		context: 25_400,
		record:  &state.Record{Issue: issue61, Waits: "CI"},
		merges:  []state.Merge{{Repo: scratchRepo, PR: 7, Lane: "main", Phase: state.Waiting}},
		events:  []state.Event{{At: at, Verb: "lease.claim", Detail: "lab-a"}},
		note:    noteAB,
		brief:   "# Count\n\n## Steps\ncount a, b, c",
	}
	p := h.prompt()
	for _, want := range []string{`You are "` + countName + `"`, "session " + oldID, "at 25k tokens", "Task: " + countTask,
		"Serves: o/r#61, waiting on CI", noteAB, "o/r#7 in lane main: waiting", "lease.claim lab-a",
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
	// An idle agent is handed over with the last task it reported idle on.
	h = handover{agent: state.Agent{Party: state.Party{Session: oldID, Name: countName}, LastTask: countTask, IdleSince: at}}
	if p := h.prompt(); !strings.Contains(p, "Task: "+countTask+"\n(The previous session had reported it idle") || h.task() != countTask {
		t.Errorf("prompt of an idle agent:\n%s", p)
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
		Records: []state.Record{{Session: old, Issue: issue61}},
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

func TestEndSessionStopsABackgroundSessionThroughItsDaemon(t *testing.T) {
	needsPlatform(t)
	for _, bg := range []bool{true, false} {
		cli := exec.Command("sleep", "60")
		if err := cli.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = cli.Wait(); close(done) }()
		var stopped []string
		orig := claudeStop
		claudeStop = func(_ context.Context, id string) error {
			if !proc.Alive(cli.Process.Pid) {
				t.Error("claude stop ran after the CLI was signalled")
			}
			stopped = append(stopped, id)
			return nil
		}
		s := &claude.Session{PID: cli.Process.Pid, ID: "test-bg", Background: bg}
		n, err := endSession(context.Background(), s)
		claudeStop = orig
		<-done
		if err != nil || n != 1 {
			t.Errorf("background %v: endSession = %d, %v", bg, n, err)
		}
		if want := map[bool]int{true: 1, false: 0}[bg]; len(stopped) != want {
			t.Errorf("background %v: claude stop ran for %v, want %d", bg, stopped, want)
		}
	}
}

// noteLauncher records the units started and has each write the agent's
// note, as the resumed turn does.
type noteLauncher struct {
	platform.Launcher
	units []platform.Unit
	write func()
}

func (l *noteLauncher) Start(u platform.Unit) error {
	l.units = append(l.units, u)
	if l.write != nil {
		l.write()
	}
	return nil
}

func (l *noteLauncher) Running(context.Context, bool, ...string) []string { return nil }

// A worker whose headless turn has ended, or whose CLI does not take the
// message by name, is asked for its note in a headless turn resumed from its
// transcript, without the desktop's reopen; with no note the hand-over says
// what the roster tells the follow-up.
func TestHandoverAsksAParkedAgentInAHeadlessTurn(t *testing.T) {
	a, out := stubApp(t)
	a.cfg.Agents.NoteWait.Duration = 3 * time.Second
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // a fake claude
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	old := state.Party{Session: oldID, Name: countName}
	dir := t.TempDir()
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: old, Task: countTask}}
		st.Starts = []state.Start{{Party: old, Mode: state.ModeBypass, Dir: dir}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	l := &noteLauncher{Launcher: plat.Launcher}
	plat.Launcher = l
	origSend := sendNote
	t.Cleanup(func() { sendNote = origSend })
	sendNote = func(context.Context, string, string, string) (peer.Result, error) {
		return peer.Result{}, peer.ErrUnreachable
	}
	h := handover{agent: state.Agent{Party: old, Task: countTask}, context: 410_000, record: &state.Record{Issue: issue61, Waits: "the supervisor's go"}}

	// No transcript: nothing to resume, the hand-over goes on without a note.
	if note, ok, err := a.askNote(t.Context(), h); err != nil || ok || note != "" || len(l.units) != 0 {
		t.Fatalf("without a transcript: %q, %v, %v, units %v", note, ok, err, l.units)
	}
	if !strings.Contains(out.String(), "cannot be resumed") {
		t.Errorf("output:\n%s", out)
	}
	projects := filepath.Join(a.cfg.Claude.ProjectsDir, "p")
	if err := os.MkdirAll(projects, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projects, oldID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l.write = func() { _ = a.store.Log(event(old, verbNote, "%s", noteAB)) }
	for _, live := range []bool{false, true} {
		out.Reset()
		l.units = nil
		h.session = nil
		if live {
			h.session = &claude.Session{ID: oldID, PID: 99, Name: countName}
		}
		note, ok, err := a.askNote(t.Context(), h)
		if err != nil || !ok || note != noteAB {
			t.Fatalf("live %v: note %q, %v, %v\n%s", live, note, ok, err, out)
		}
		if len(l.units) != 1 {
			t.Fatalf("live %v: units %+v", live, l.units)
		}
		u := l.units[0]
		argv := strings.Join(u.Argv, " ")
		if !strings.HasPrefix(u.Name, wakePrefix(oldID)) || u.Dir != dir || u.StopPost != nil || u.StopTimeout != turnStopWait || u.MaxRuntime != 0 ||
			!strings.Contains(argv, "-p --resume "+oldID+" --permission-mode "+state.ModeBypass) || !strings.Contains(argv, "agents note") {
			t.Errorf("live %v: unit %+v", live, u)
		}
		want := map[bool]string{false: "does not run: resuming", true: "did not take the message by name"}[live]
		if !strings.Contains(out.String(), want) || !strings.Contains(out.String(), "note: written after") {
			t.Errorf("live %v: output:\n%s", live, out)
		}
	}

	// The resumed turn writes no note: the follow-up rests on the roster,
	// which names the served issue and what the predecessor waited on.
	l.write = nil
	a.cfg.Agents.NoteWait.Duration = 100 * time.Millisecond
	h.agent.Party = state.Party{Session: oldID, Name: "test: silent"}
	if _, ok, err := a.askNote(t.Context(), h); ok || err != nil {
		t.Fatalf("a silent turn: %v, %v", ok, err)
	}
	if got := h.roster(); got != "serves o/r#61, waiting on the supervisor's go" {
		t.Errorf("roster = %q", got)
	}
	if p := h.prompt(); !strings.Contains(p, "Serves: o/r#61, waiting on the supervisor's go") {
		t.Errorf("prompt:\n%s", p)
	}
	if got := (handover{}).roster(); !strings.Contains(got, "no sessions serve record") {
		t.Errorf("roster without a record = %q", got)
	}
}
