package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
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
