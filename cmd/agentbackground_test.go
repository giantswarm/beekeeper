package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	bgID      = "6f1c2a90-4b7e-4d21-9c0a-3e5f7a8b9c10"
	bgShortID = "6f1c2a90"
	bgName    = "FIX beekeeper 1670"
	bgTask    = "Brief: start background sessions"
	bgModel   = "opus"
)

// bgCall is one claude --bg call the stub saw.
type bgCall struct {
	dir       string
	env, argv []string
}

// stubBackground replaces the claude --bg calls and the daemon's listing:
// each call prints out, and the daemon lists rows. A fake claude on PATH
// gives the calls a binary.
func stubBackground(t *testing.T, out string, rows ...bgSession) *[]bgCall {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // a fake claude
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	var calls []bgCall
	origRun, origList, origPoll := bgRun, bgList, bgPoll
	t.Cleanup(func() { bgRun, bgList, bgPoll = origRun, origList, origPoll })
	bgRun = func(_ context.Context, dir string, env, argv []string) (string, error) {
		calls = append(calls, bgCall{dir: dir, env: env, argv: argv})
		return out, nil
	}
	bgList = func(context.Context, string, []string, bool) ([]bgSession, error) { return rows, nil }
	bgPoll = time.Millisecond
	return &calls
}

func TestBackgroundArgv(t *testing.T) {
	got := bgStartArgv("/bin/claude", bgName, bgModel, "the brief", "--tools", tToolBash)
	want := []string{"/bin/claude", bgFlag, remoteControlFlag, bgName, permissionModeFlag, state.ModeBypass, "-n", bgName, modelFlag, bgModel, "--tools", tToolBash, "--", "the brief"}
	if !slices.Equal(got, want) {
		t.Errorf("start argv = %q\nwant %q", got, want)
	}
	// A resume passes no flag, which would start a copy: the session keeps
	// its own Remote Control, mode, name and model.
	if got := bgResumeArgv("/bin/claude", bgID, "-a message"); !slices.Equal(got, []string{"/bin/claude", bgFlag, resumeFlag, bgID, " -a message"}) {
		t.Errorf("resume argv = %q", got)
	}
	if got := bgShort("note: woke session 6f1c2a90 with its saved options.\nbackgrounded · 6f1c2a90 · x\n  claude agents  list sessions\n"); got != bgShortID {
		t.Errorf("bgShort = %q", got)
	}
	if got := bgShort("Workspace not trusted."); got != "" {
		t.Errorf("bgShort without a session = %q", got)
	}
}

func TestCleanEnvDropsTheCallersSession(t *testing.T) {
	env := cleanEnv([]string{"PATH=/usr/bin", "HOME=/home/x", "CLAUDE_CODE_SESSION_ID=caller", "CLAUDE_CODE_SESSION_NAME=Supervisor", "CLAUDECODE=1", "CCD_X=1", "BEEKEEPER_CONFIG=/old"}, "/c.yaml")
	for _, kv := range env {
		if strings.HasPrefix(kv, "CLAUDE") || strings.HasPrefix(kv, "CCD") || kv == "BEEKEEPER_CONFIG=/old" {
			t.Errorf("cleanEnv kept %q", kv)
		}
	}
	for _, kv := range []string{"HOME=/home/x", "BEEKEEPER_CONFIG=/c.yaml"} {
		if !slices.Contains(env, kv) {
			t.Errorf("cleanEnv dropped %q: %q", kv, env)
		}
	}
}

func TestAgentsModeIsValidated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	for mode, ok := range map[string]bool{"": true, config.ModeDesktop: true, config.ModeBackground: true, "bg": false} {
		if err := os.WriteFile(path, []byte("stateDir: "+dir+"\nagents: {mode: \""+mode+"\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := config.Load(path)
		if (err == nil) != ok {
			t.Errorf("agents.mode %q: err = %v", mode, err)
		}
		if err == nil && mode == "" && c.Agents.Mode != config.ModeDesktop {
			t.Errorf("default agents.mode = %q", c.Agents.Mode)
		}
	}
}

// A background start runs claude --bg with Remote Control on the
// configured model and none of the caller's CLAUDE* variables, has the
// session on the roster under its own name, busy with its task, when it
// returns, and imports nothing: no unit, no reopen.
func TestStartBackgroundIsOnTheRosterAtOnce(t *testing.T) {
	a, _ := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	l := &startRecorder{Launcher: plat.Launcher}
	useLauncher(t, l)
	a.cfg.Agents.Mode, a.cfg.Agents.Model = config.ModeBackground, bgModel
	t.Setenv("CLAUDE_CODE_SESSION_ID", "caller")
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "local_caller")
	calls := stubBackground(t, "backgrounded · "+bgShortID+" · "+bgName+"\n", bgSession{ID: "0ther", SessionID: "0ther-id"}, bgSession{ID: bgShortID, SessionID: bgID, Name: bgName})
	dir := t.TempDir()
	by := state.Party{Session: "sv", Name: claimSupervisor}

	sa, err := a.startAgent(t.Context(), agentStart{name: bgName, role: workerRole, brief: "the brief", task: bgTask, serves: "giantswarm/beekeeper#1670", dir: dir, by: &by})
	if err != nil {
		t.Fatal(err)
	}
	if !sa.background || sa.id != bgID || sa.want != bgModel || sa.task != bgTask {
		t.Errorf("started %+v", sa)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls %+v", *calls)
	}
	c := (*calls)[0]
	if c.dir != dir || !slices.Equal(c.argv[1:], bgStartArgv("claude", bgName, bgModel, "the brief")[1:]) {
		t.Errorf("call %+v", c)
	}
	if i := slices.IndexFunc(c.env, func(kv string) bool { return strings.HasPrefix(kv, "CLAUDE") }); i >= 0 {
		t.Errorf("the caller's %q reached the session", c.env[i])
	}
	if len(l.started) > 0 {
		t.Errorf("units %v: a background start imports nothing", l.started)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	i := st.StartIndex(state.Party{Session: bgID})
	if i < 0 || !st.Starts[i].Background || st.Starts[i].HostSession != "" || st.Starts[i].Model != bgModel || st.Starts[i].Name != bgName {
		t.Fatalf("starts %+v", st.Starts)
	}
	j := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Session == bgID })
	if j < 0 || st.Agents[j].Name != bgName || st.Agents[j].Task != bgTask || st.Agents[j].HostSession != "" {
		t.Errorf("roster %+v", st.Agents)
	}
	if !a.backgroundAgent(t.Context(), st, st.Agents[j].Party) || desktopStarted(st, st.Agents[j].Party) {
		t.Errorf("a background start counts as a desktop one")
	}
	if _, ok := rowlessCandidate(st, st.Agents[j], time.Now().Add(time.Hour)); ok {
		t.Errorf("a background start is a rowless candidate")
	}
}

// A start whose claude --bg names no session fails and records nothing.
func TestStartBackgroundWithoutASession(t *testing.T) {
	a, _ := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	stubBackground(t, "Workspace not trusted.\n")
	by := state.Party{Session: "sv", Name: claimSupervisor}
	_, err := a.startAgent(t.Context(), agentStart{name: bgName, brief: "b", task: bgTask, dir: t.TempDir(), by: &by, mode: config.ModeBackground})
	if err == nil || !strings.Contains(err.Error(), "Workspace not trusted") {
		t.Errorf("err = %v", err)
	}
	st, _ := a.store.Read()
	if len(st.Starts) != 0 {
		t.Errorf("starts %+v", st.Starts)
	}
}

// A background session with no running CLI is woken by claude --bg
// --resume with the message alone, never by the desktop; a resume that
// started a copy instead fails, naming it.
func TestWakeBackground(t *testing.T) {
	a, out := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	l := &startRecorder{Launcher: plat.Launcher}
	useLauncher(t, l)
	dir := t.TempDir()
	writeFile(t, filepath.Join(a.cfg.Claude.ProjectsDir, "-w", bgID+".jsonl"), `{"type":"user","cwd":"`+dir+`"}`+"\n")
	p := state.Party{Session: bgID, Name: bgName}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{Party: p, Mode: state.ModeBypass, Dir: dir, At: time.Now(), Background: true})
		st.Agents = append(st.Agents, state.Agent{Party: p, Task: bgTask})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	calls := stubBackground(t, "note: woke session "+bgShortID+" with its saved options.\nbackgrounded · "+bgShortID+" · "+bgName+"\n")
	if err := a.wakeAgent(t.Context(), state.Party{Name: supervisorName}, bgName, "your task", ""); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || !slices.Equal((*calls)[0].argv[1:], []string{bgFlag, resumeFlag, bgID, "your task"}) || (*calls)[0].dir != dir {
		t.Errorf("calls %+v", *calls)
	}
	if len(l.started) > 0 || !strings.Contains(out.String(), "resumed background session "+bgID) {
		t.Errorf("units %v, output:\n%s", l.started, out)
	}

	stubBackground(t, "backgrounded · 9e3d7b10 · "+bgName+"\n")
	if err := a.wakeAgent(t.Context(), state.Party{Name: supervisorName}, bgName, "your task", ""); err == nil || !strings.Contains(err.Error(), "started a copy 9e3d7b10") {
		t.Errorf("a copy: err = %v", err)
	}
}

// agents desktop refuses a background session: it has no desktop turn.
func TestAgentsDesktopRefusesABackgroundSession(t *testing.T) {
	a, _ := stubApp(t)
	a.cfg.Agents.Mode = config.ModeBackground
	rosterAgent(t, a, state.Agent{Party: state.Party{Session: bgID, Name: bgName}, Task: bgTask})
	stubBackground(t, "", bgSession{ID: bgShortID, SessionID: bgID, Kind: bgKind})
	c := a.agentDesktopCmd()
	c.SetArgs([]string{bgName})
	c.SetContext(t.Context())
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "background session") {
		t.Errorf("err = %v", err)
	}
}

// A background successor's relay names its run until the start records
// the id the claude daemon chose.
func TestTakesRelayByRunName(t *testing.T) {
	to := state.Party{Name: "Supervisor run 4"}
	me := state.Party{Session: bgID, Name: "Supervisor run 4"}
	if !takesRelay(to, me) || takesRelay(to, state.Party{Session: bgID, Name: "Supervisor run 5"}) {
		t.Error("a name-only relay")
	}
	if takesRelay(state.Party{Session: "x", Name: me.Name}, me) || !takesRelay(state.Party{Session: bgID}, me) {
		t.Error("a relay with an id matches by the id")
	}
}

// A background successor starts as a background session; its relay names
// its run, then the id the claude daemon chose, and its brief has it take
// the role and go on in the same turn.
func TestBackgroundSuccessor(t *testing.T) {
	a, _ := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	a.now = relayNow
	a.cfg.Agents.Mode = config.ModeBackground
	a.cfg.Supervisor.RelayTTL.Duration = 15 * time.Minute
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { *st = *handOverState(); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	calls := stubBackground(t, "backgrounded · "+bgShortID+" · Supervisor run 12\n", bgSession{ID: bgShortID, SessionID: bgID})
	to, _, err := a.startSuccessor(t.Context(), supervisorRole, supA, supA, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if to.Session != bgID || to.HostSession != "" || to.Name != supervisorRole.runName(12) {
		t.Errorf("successor %+v", to)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls %+v", *calls)
	}
	argv := (*calls)[0].argv
	if brief := argv[len(argv)-1]; !strings.Contains(brief, "`beekeeper supervisor start`") || strings.Contains(brief, "end the turn") || strings.Contains(brief, "desktop") {
		t.Errorf("brief:\n%s", brief)
	}
	st, _ := a.store.Read()
	if st.Relay == nil || st.Relay.To.Session != bgID || st.Relay.To.Name != to.Name {
		t.Errorf("relay %+v", st.Relay)
	}
	if i := st.StartIndex(to); i < 0 || !st.Starts[i].Background {
		t.Errorf("starts %+v", st.Starts)
	}
}

// A hand-over in background mode starts the follow-up as a background
// session: its roster row has no desktop id, and the hand-over says
// nothing of a desktop, a title or a claude:// link.
func TestHandoverInBackgroundMode(t *testing.T) {
	a, out := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	l := &startRecorder{Launcher: plat.Launcher}
	useLauncher(t, l)
	a.cfg.Agents.Mode, a.cfg.Agents.NoteWait.Duration = config.ModeBackground, 100*time.Millisecond
	writeFile(t, filepath.Join(filepath.Dir(a.cfg.Claude.ProjectsDir), "settings.json"),
		`{"hooks":{"PermissionRequest":[{"matcher":"*","hooks":[{"type":"command","command":"beekeeper hook permissionrequest"}]}]}}`)
	dir := t.TempDir()
	old := state.Party{Session: "0ld-background", Name: bgName}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{Party: old, Mode: state.ModeBypass, Dir: dir, At: time.Now(), Background: true})
		st.Agents = append(st.Agents, state.Agent{Party: old, Task: bgTask})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	stubBackground(t, "backgrounded · "+bgShortID+" · "+bgName+"\n", bgSession{ID: bgShortID, SessionID: bgID, Kind: bgKind})
	if err := a.handOver(t.Context(), handover{agent: state.Agent{Party: old, Task: bgTask}, dir: dir}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, not := range []string{"local_", "claude://", "desktop", "set_model"} {
		if strings.Contains(out.String(), not) {
			t.Errorf("the hand-over says %q:\n%s", not, out)
		}
	}
	if !strings.Contains(out.String(), "background session "+bgID) {
		t.Errorf("output:\n%s", out)
	}
	if len(l.started) > 0 {
		t.Errorf("units %v", l.started)
	}
	st, _ := a.store.Read()
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == bgName })
	if i < 0 || st.Agents[i].Session != bgID || st.Agents[i].HostSession != "" || st.Agents[i].Task != bgTask {
		t.Errorf("roster %+v", st.Agents)
	}
}

// Under agents.mode background a registered session without a desktop
// session takes the background route only when the claude daemon ran it:
// a session run in a terminal keeps the headless one.
func TestBackgroundAgentAsksTheDaemon(t *testing.T) {
	a, _ := stubApp(t)
	st := &state.State{}
	p := state.Party{Session: bgID, Name: bgName}
	stubBackground(t, "", bgSession{ID: bgShortID, SessionID: bgID, Kind: bgKind}, bgSession{ID: "7e2", SessionID: "7e2-terminal", Kind: "interactive"})
	if a.backgroundAgent(t.Context(), st, p) {
		t.Error("desktop mode takes the background route")
	}
	a.cfg.Agents.Mode = config.ModeBackground
	if !a.backgroundAgent(t.Context(), st, p) {
		t.Error("a session the daemon ran")
	}
	if a.backgroundAgent(t.Context(), st, state.Party{Session: "7e2-terminal", Name: "terminal"}) {
		t.Error("a terminal session takes the background route")
	}
	if a.backgroundAgent(t.Context(), st, state.Party{Session: bgID, HostSession: "local_" + bgID}) {
		t.Error("a desktop session takes the background route")
	}
}

// --mode background is refused beside --desktop or --harness omp, which it
// would silently override or be ignored by.
func TestModeBackgroundRefusesDesktopAndOmp(t *testing.T) {
	brief := filepath.Join(t.TempDir(), "brief.md")
	writeFile(t, brief, "# a task\n")
	for _, extra := range [][]string{{"--desktop"}, {"--harness", "omp"}} {
		a, _ := stubApp(t)
		c := a.agentStartCmd()
		c.SetArgs(append([]string{bgName, brief, modeFlag, config.ModeBackground}, extra...))
		c.SetContext(t.Context())
		var ee *exitError
		if err := c.Execute(); !errors.As(err, &ee) || ee.code != ExitUsage || !strings.Contains(ee.msg, extra[0]) {
			t.Errorf("%v: err = %v", extra, err)
		}
	}
}

// agents remove stops a background session beekeeper started (claude
// stop) and leaves a session its person started running.
func TestAgentsRemoveStopsABackgroundStart(t *testing.T) {
	a, out := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	p := state.Party{Session: bgID, Name: bgName}
	mine := state.Party{Session: "7a2b3c4d-0000-4000-8000-000000000001", Name: "person's session"}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{Party: p, Mode: state.ModeBypass, Dir: t.TempDir(), At: time.Now(), Background: true})
		st.Agents = append(st.Agents, state.Agent{Party: p, Task: bgTask}, state.Agent{Party: mine, Task: bgTask})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	remove := func(name string) {
		t.Helper()
		c := a.agentsCmd()
		c.SetArgs([]string{loopRemove, name})
		c.SetContext(t.Context())
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
	}
	calls := stubBackground(t, "")
	remove(bgName)
	if len(*calls) != 1 || !slices.Equal((*calls)[0].argv[1:], []string{bgStopCmd, bgShortID}) {
		t.Errorf("calls %+v", *calls)
	}
	if !strings.Contains(out.String(), "stopped its background session "+bgShortID) {
		t.Errorf("output:\n%s", out)
	}
	*calls = nil
	remove(mine.Name)
	if len(*calls) != 0 {
		t.Errorf("a person's session was stopped: %+v", *calls)
	}
	if st, _ := a.store.Read(); len(st.Agents) != 0 {
		t.Errorf("roster %+v", st.Agents)
	}
	// The supervisor's own background session stays.
	sv := state.Party{Session: "s1", Name: agentOne}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{Party: sv, Mode: state.ModeBypass, Dir: t.TempDir(), At: time.Now(), Background: true})
		st.Agents = append(st.Agents, state.Agent{Party: sv, Task: bgTask})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	remove(agentOne)
	if len(*calls) != 0 || !strings.Contains(out.String(), "supervisor's or the guide's role") {
		t.Errorf("the supervisor's session: calls %+v, output:\n%s", *calls, out)
	}
}

// parkedResident names a parked background start whose turn ended, and
// leaves a session in a turn, one not running, one its person started, a
// resumable park, an agent on an execution host and the supervisor alone.
func TestParkedResident(t *testing.T) {
	now := time.Now()
	idle := now.Add(-time.Hour)
	park := &state.Park{At: idle, Waits: "the review"}
	mk := func(n int) state.Party {
		return state.Party{Session: fmt.Sprintf("6f1c2a90-0000-4000-8000-%012d", n), Name: fmt.Sprintf("agent %d", n)}
	}
	st := &state.State{Supervisor: &state.Supervisor{Party: state.Party{Name: agentOne, Session: "s1"}}}
	var sessions []*claude.Session
	add := func(p state.Party, bg bool, ag state.Agent, s *claude.Session) {
		st.Starts = append(st.Starts, state.Start{Party: p, Background: bg})
		ag.Party, ag.Task = p, bgTask
		st.Agents = append(st.Agents, ag)
		if s != nil {
			s.ID, s.Name = p.Session, p.Name
			sessions = append(sessions, s)
		}
	}
	stopped := mk(1)
	add(stopped, true, state.Agent{Park: park}, &claude.Session{LastActive: idle})
	add(mk(2), true, state.Agent{Park: park}, &claude.Session{LastActive: now.Add(-time.Second)}) // just parked: its turn ends
	add(mk(3), true, state.Agent{Park: park}, &claude.Session{LastActive: idle, Commands: []claude.Command{{PID: 9}}})
	add(mk(4), true, state.Agent{Park: park}, &claude.Session{LastActive: idle, LastSubagent: now})
	add(mk(5), true, state.Agent{Park: park}, nil) // already stopped
	add(mk(6), false, state.Agent{Park: park}, &claude.Session{LastActive: idle})
	add(mk(7), true, state.Agent{}, &claude.Session{LastActive: idle}) // not parked
	add(mk(8), true, state.Agent{Park: &state.Park{At: idle, Resumable: now}}, &claude.Session{LastActive: idle})
	add(mk(9), true, state.Agent{Park: park, Execution: &execution.Ref{}}, &claude.Session{LastActive: idle})
	add(st.Supervisor.Party, true, state.Agent{Park: park}, &claude.Session{LastActive: idle})
	got := parkedResident(st, sessions, &proc.Table{ByPID: map[int]*proc.Process{}}, now)
	if len(got) != 1 || !got[0].Is(stopped) {
		t.Errorf("parkedResident = %+v, want only %s", got, stopped.Name)
	}
}

// argvs is the command lines of calls.
func argvs(calls []bgCall) [][]string {
	out := make([][]string, len(calls))
	for i, c := range calls {
		out[i] = c.argv
	}
	return out
}

// parkedBackground puts the background start p, parked on a desk review,
// with its transcript in dir, on a's roster.
func parkedBackground(t *testing.T, a *app, p state.Party, dir string) {
	t.Helper()
	// the transcript the resume continues
	writeFile(t, filepath.Join(a.cfg.Claude.ProjectsDir, "-w", p.Session+".jsonl"), `{"type":"user","cwd":"`+dir+`"}`+"\n")
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{Party: p, Mode: state.ModeBypass, Dir: dir, At: time.Now(), Background: true})
		st.Agents = append(st.Agents, state.Agent{Party: p, Task: bgTask})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if line, err := a.park(p, "", "the desk review"); err != nil || !strings.Contains(line, "The watch stops your background session once the turn ends") {
		t.Fatalf("park: %q, %v", line, err)
	}
}

// stubFresh makes *live the sessions a stop's last look reads.
func stubFresh(t *testing.T, live *[]*claude.Session) {
	t.Helper()
	was := freshSessions
	t.Cleanup(func() { freshSessions = was })
	freshSessions = func(*app) ([]*claude.Session, *proc.Table, error) {
		return *live, &proc.Table{ByPID: map[int]*proc.Process{}}, nil
	}
}

// pollStop runs the watch's stop step on the state read now and live.
func pollStop(t *testing.T, w *watcher, live []*claude.Session) {
	t.Helper()
	st, err := w.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	w.stopParked(t.Context(), st, live)
}

// A parked background session costs no memory: once its turn ended the
// watch stops it (claude stop, once), and the park's resume continues it
// from its transcript (claude --bg --resume) with what settled the wait.
func TestParkedBackgroundSessionStopsAndResumes(t *testing.T) {
	a, out := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	dir := t.TempDir()
	p := state.Party{Session: bgID, Name: bgName}
	parkedBackground(t, a, p, dir)
	w := &watcher{app: a, last: map[string]time.Time{}, reported: map[string]bool{}}
	live := []*claude.Session{{ID: bgID, Name: bgName, Background: true, LastActive: a.now}}
	stubFresh(t, &live)
	calls := stubBackground(t, "")
	pollStop(t, w, live) // its park's turn has not ended yet
	if len(*calls) != 0 {
		t.Fatalf("stopped in its turn: %q", argvs(*calls))
	}
	w.now = a.now.Add(time.Minute)
	pollStop(t, w, live)
	if len(*calls) != 1 || !slices.Equal((*calls)[0].argv[1:], []string{bgStopCmd, bgShortID}) {
		t.Fatalf("calls %q", argvs(*calls))
	}
	if !strings.Contains(out.String(), `PARKED SESSION STOPPED "`+bgName+`"`) {
		t.Errorf("output:\n%s", out)
	}
	if evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == parkStoppedEvent }); err != nil || len(evs) != 1 {
		t.Errorf("events %+v, %v", evs, err)
	}
	*calls, live = nil, nil
	pollStop(t, w, nil) // stopped: its CLI is gone
	if len(*calls) != 0 {
		t.Errorf("stopped twice: %q", argvs(*calls))
	}

	calls = stubBackground(t, "note: woke session "+bgShortID+" with its saved options.\nbackgrounded · "+bgShortID+" · "+bgName+"\n")
	if err := a.resumeParked(t.Context(), state.Party{Name: supervisorName}, bgName); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || !slices.Equal((*calls)[0].argv[1:3], []string{bgFlag, resumeFlag}) || (*calls)[0].argv[3] != bgID ||
		!strings.Contains((*calls)[0].argv[4], "the desk review") || (*calls)[0].dir != dir {
		t.Errorf("resume calls %q", argvs(*calls))
	}
}

// A resume that lands between the watch's look and its stop (it ended the
// park and sent its message to the running CLI) keeps the session running:
// the stop looks again under the session's lock and finds no park.
func TestParkedBackgroundStopAfterAResume(t *testing.T) {
	a, _ := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	p := state.Party{Session: bgID, Name: bgName}
	parkedBackground(t, a, p, t.TempDir())
	a.now = a.now.Add(time.Minute)
	w := &watcher{app: a, last: map[string]time.Time{}, reported: map[string]bool{}}
	live := []*claude.Session{{ID: bgID, Name: bgName, Background: true, PID: 4242, LastActive: a.now.Add(-time.Hour)}}
	stubFresh(t, &live)
	calls := stubBackground(t, "")
	seen, err := a.store.Read() // the watch's look: parked and quiet
	if err != nil {
		t.Fatal(err)
	}
	woke := stubWakes(t) // the resume reaches the running CLI by name
	if err := a.resumeParked(t.Context(), state.Party{Name: supervisorName}, bgName); err != nil || len(*woke) != 1 {
		t.Fatalf("resume: %v, woke %v", err, *woke)
	}
	w.stopParked(t.Context(), seen, live)
	if len(*calls) != 0 {
		t.Errorf("the stop ended the CLI the resume woke: %q", argvs(*calls))
	}
	// Parked again on another wait, quiet: the look at the old park stops nothing either.
	if _, err := a.park(p, "", "the next review"); err != nil {
		t.Fatal(err)
	}
	w.stopParked(t.Context(), seen, live)
	if len(*calls) != 0 {
		t.Errorf("stopped on a park it did not see: %q", argvs(*calls))
	}
	pollStop(t, w, live)
	if len(*calls) != 1 {
		t.Errorf("the new park's stop: %q", argvs(*calls))
	}
}

// A wake that comes while the stop runs waits for it, then resumes the
// stopped session (claude --bg --resume) instead of sending to the CLI the
// stop ends; a wake just made keeps the session running for stewardQuiet.
func TestParkedBackgroundWakeDuringTheStop(t *testing.T) {
	a, _ := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	p := state.Party{Session: bgID, Name: bgName}
	parkedBackground(t, a, p, t.TempDir())
	w := &watcher{app: a, last: map[string]time.Time{}, reported: map[string]bool{}}
	live := []*claude.Session{{ID: bgID, Name: bgName, Background: true, LastActive: a.now.Add(-time.Hour)}}
	stubFresh(t, &live)
	stubBackground(t, "")
	var mu sync.Mutex
	var seq []string
	var woken bool
	done := make(chan error, 1)
	bgRun = func(_ context.Context, _ string, _, argv []string) (string, error) {
		mu.Lock()
		seq = append(seq, argv[1])
		mu.Unlock()
		if argv[1] != bgStopCmd {
			return "backgrounded · " + bgShortID + " · " + bgName + "\n", nil
		}
		if woken {
			live = nil
			return "", nil
		}
		woken = true
		quiet := *a
		quiet.out = io.Discard
		go func() {
			done <- quiet.wakeAgent(context.Background(), state.Party{Name: supervisorName}, bgName, "your review", "")
		}()
		time.Sleep(200 * time.Millisecond) // the wake runs meanwhile, and waits
		mu.Lock()
		defer mu.Unlock()
		seq = append(seq, "stopped")
		live = nil
		return "", nil
	}
	pollStop(t, w, live)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if want := []string{bgStopCmd, "stopped", bgFlag}; !slices.Equal(seq, want) {
		t.Errorf("calls %v, want %v", seq, want)
	}

	// Running again after the wake, quiet: the wake's stamp holds the stop off.
	live = []*claude.Session{{ID: bgID, Name: bgName, Background: true, LastActive: a.now.Add(-time.Hour)}}
	seq = nil
	a.now = time.Now()
	pollStop(t, w, live)
	if len(seq) != 0 {
		t.Errorf("stopped just after its wake: %v", seq)
	}
	a.now = a.now.Add(time.Minute)
	pollStop(t, w, live)
	if !slices.Equal(seq, []string{bgStopCmd}) {
		t.Errorf("calls %v", seq)
	}
}

// A stop that fails leaves the session parked and running, says so, and is
// tried again on the next poll.
func TestParkedBackgroundStopFails(t *testing.T) {
	a, out := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	p := state.Party{Session: bgID, Name: bgName}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{Party: p, Dir: t.TempDir(), Background: true})
		st.Agents = append(st.Agents, state.Agent{Party: p, Task: bgTask, Park: &state.Park{Waits: "x"}})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	stubBackground(t, "")
	var tries int
	bgRun = func(context.Context, string, []string, []string) (string, error) {
		tries++
		return "no such session", errors.New("exit status 1")
	}
	w := &watcher{app: a, last: map[string]time.Time{}, reported: map[string]bool{}}
	live := []*claude.Session{{ID: bgID, Name: bgName, Background: true, LastActive: a.now.Add(-time.Hour)}}
	stubFresh(t, &live)
	for range 2 {
		pollStop(t, w, live)
	}
	if tries != 2 || !strings.Contains(out.String(), `PARKED SESSION STAYS "`+bgName+`"`) || !strings.Contains(out.String(), "no such session") {
		t.Errorf("tries %d, output:\n%s", tries, out)
	}
}
