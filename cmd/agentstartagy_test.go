//go:build unix

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	testAgyWorker = "agy worker"
	testAgyTask   = "ship the harness"
)

// An agy agent's model is named with --model and validated against the exact
// selectors agy models lists: there is no harness default.
func TestAgyModel(t *testing.T) {
	const opus, gemini = "claude-opus-4-6-thinking", "gemini-3-1-pro-medium"
	listed := func() ([]string, error) { return []string{opus, gemini}, nil }
	for _, tc := range []struct {
		flag, want, err string
	}{
		{flag: opus, want: opus},
		{flag: gemini, want: gemini},
		{err: "needs its model"},
		{flag: "unknown-model", err: `lists no model "unknown-model"`},
	} {
		got, err := agyModel(tc.flag, listed)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("agyModel(%q) = %q, %v; want error %q", tc.flag, got, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("agyModel(%q) = %q, %v; want %q", tc.flag, got, err, tc.want)
		}
	}
	if _, err := agyModel(opus, func() ([]string, error) { return nil, errors.New("agy failed") }); err == nil {
		t.Error("an unreadable model list passed")
	}
	argv := agyArgv("agy", opus, true, "proj-1")
	if !slices.Contains(argv, "--remote-control") || !slices.Contains(argv, "proj-1") {
		t.Errorf("agyArgv = %v", argv)
	}
	runArgv := agyRunArgv("/bin/beekeeper", "/state/agy/id1.in", "/state/agy/id1.log", argv)
	if !slices.Contains(runArgv, "agy-run") || !slices.Contains(runArgv, "/state/agy/id1.in") {
		t.Errorf("agyRunArgv = %v", runArgv)
	}
}

// An agy agent's beekeeper commands act as its roster entry through
// BEEKEEPER_AGY_AGENT and BEEKEEPER_AGENT_NAME, even where its tool shell
// carries another session's variables: agents idle --done --report,
// agents park, log add and note add work without --as.
func TestAgyAgentIsTheCaller(t *testing.T) {
	stubIssuePark(t, "giantswarm/beekeeper#1")
	const id = "8765dcba-0000-4000-8000-000000000000"
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "some-other-session")
	t.Setenv(agy.EnvAgent, id)
	t.Setenv(agy.EnvName, testAgyWorker)
	var out bytes.Buffer
	a := &app{cfg: &config.Config{StateDir: dir}, store: store, now: time.Now(), out: &out}
	me, err := a.caller()
	if err != nil {
		t.Fatal(err)
	}
	ag := state.Agent{Party: state.Party{HostSession: agy.HostPrefix + id, Name: testAgyWorker}}
	if !ag.Is(me) || me.Session != "" {
		t.Fatalf("caller = %+v, want the agy agent's roster entry", me)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: ag.Party, Task: "ship feature", Registered: a.now}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	runCmd := func(args ...string) error {
		c := a.agentsCmd()
		c.SetOut(&out)
		c.SetErr(&out)
		c.SetArgs(args)
		return c.Execute()
	}
	if err := runCmd("park", "--on", "giantswarm/beekeeper#1", "waiting on CI"); err != nil {
		t.Fatalf("agents park: %v", err)
	}
	if err := runCmd("idle", "--done", "--report", "shipped feature", "--problem", "none"); err != nil {
		t.Fatalf("agents idle --done: %v", err)
	}
	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 1 || !st.Agents[0].Done || st.Agents[0].Task != "" {
		t.Fatalf("roster after idle --done = %+v", st.Agents)
	}
}

type agyTestLauncher struct {
	platform.Launcher
	mu      sync.Mutex
	started []platform.Unit
	stopped []string
	states  map[string]string
	onStart func(u platform.Unit)
}

func (l *agyTestLauncher) Available() bool { return true }

func (l *agyTestLauncher) Start(u platform.Unit) error {
	l.mu.Lock()
	l.started = append(l.started, u)
	if l.states == nil {
		l.states = map[string]string{}
	}
	l.states[u.Name] = unitActive
	cb := l.onStart
	l.mu.Unlock()
	if cb != nil {
		cb(u)
	}
	return nil
}

func (l *agyTestLauncher) State(_ context.Context, name string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.states[name]
}

func (l *agyTestLauncher) Stop(_ context.Context, name string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopped = append(l.stopped, name)
	if l.states != nil {
		l.states[name] = "inactive"
	}
	return nil
}

func writeFakeAgy(t *testing.T, modelsOut string) string {
	t.Helper()
	binDir := t.TempDir()
	script := filepath.Join(binDir, "agy")
	body := "#!/bin/sh\nif [ \"$1\" = \"models\" ]; then\ncat <<'EOF'\n" + modelsOut + "\nEOF\nexit 0\nfi\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return script
}

// startAgyAgent validates --model against agy models, registers the agent on
// the roster as agy_<id>, starts the systemd unit with BEEKEEPER_AGY_AGENT
// and BEEKEEPER_AGENT_NAME, writes the worker rules + brief to the FIFO inbox,
// and confirms the brief arrived in its own conversation (init + user_input
// step_update in <stateDir>/agy/<id>.log).
func TestStartAgyAgentDeliversBriefAndConfirmsConversation(t *testing.T) {
	const (
		convID = "conv-9988-7766"
		model  = "claude-opus-4-6-thinking"
	)
	writeFakeAgy(t, "Available models:\n  "+model+" (current)\n  gemini-3-1-pro-medium\n")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sup-1")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", "supervisor")

	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{
		cfg: &config.Config{
			StateDir: dir,
			Agy: config.Agy{
				BrainDir:         filepath.Join(dir, "brain"),
				ProjectsDir:      filepath.Join(dir, "projects"),
				ConversationsDir: filepath.Join(dir, "conversations"),
			},
		},
		store: store,
		now:   time.Now(),
		out:   &out,
	}

	var gotBrief string
	origLauncher, origMachine := plat.Launcher, plat.Machine
	t.Cleanup(func() { plat.Launcher, plat.Machine = origLauncher, origMachine })
	plat.Machine = tableMachine{plat.Machine}
	l := &agyTestLauncher{
		onStart: func(u platform.Unit) {
			go func() {
				var startID string
				for _, e := range u.Env {
					if v, ok := strings.CutPrefix(e, agy.EnvAgent+"="); ok {
						startID = v
					}
				}
				inbox := agy.InboxPath(dir, startID)
				logPath := agy.StreamPath(dir, startID)
				f, err := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // test FIFO
				if err != nil {
					return
				}
				defer func() { _ = f.Close() }()
				_, _ = bufio.NewReader(f).ReadString('\n')
				if msgs, derr := agy.Drain(inbox); derr == nil && len(msgs) > 0 {
					gotBrief = msgs[0]
				}
				stream := `{"event":"init","conversation_id":"` + convID + `"}` + "\n" +
					`{"event":"step_update","step_update":{"conversation_id":"` + convID + `","step_type":"user_input"}}` + "\n"
				_ = os.WriteFile(logPath, []byte(stream), 0o600)
			}()
		},
	}
	plat.Launcher = l

	sp := agentStart{
		name:  testAgyWorker,
		brief: "Read the issue and ship it.",
		task:  testAgyTask,
		model: model,
		dir:   dir,
	}
	if err := a.startAgyAgent(context.Background(), sp); err != nil {
		t.Fatalf("startAgyAgent: %v", err)
	}
	if !strings.Contains(out.String(), "conversation "+convID) || !strings.Contains(out.String(), "on "+model) {
		t.Fatalf("output = %q", out.String())
	}
	if !strings.Contains(gotBrief, "Read the issue and ship it.") {
		t.Fatalf("FIFO brief = %q, want brief text", gotBrief)
	}
	if len(l.started) != 1 {
		t.Fatalf("started units = %d, want 1", len(l.started))
	}
	u := l.started[0]
	if !slices.Contains(u.Env, agy.EnvName+"="+testAgyWorker) {
		t.Errorf("unit env = %v, want %s", u.Env, agy.EnvName)
	}
	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 1 || !strings.HasPrefix(st.Agents[0].HostSession, agy.HostPrefix) || st.Agents[0].Task != testAgyTask || st.Agents[0].Undelivered != "" {
		t.Fatalf("roster = %+v", st.Agents)
	}
}

// When the agy unit exits before confirming the brief in its conversation,
// startAgyAgent stops the unit, marks the roster entry undelivered, and
// returns an error.
func TestStartAgyAgentMarksUndeliveredWhenUnitEndsBeforeBrief(t *testing.T) {
	const model = "claude-opus-4-6-thinking"
	writeFakeAgy(t, "Available models:\n  "+model+"\n")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sup-1")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", "supervisor")

	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{
		cfg:   &config.Config{StateDir: dir},
		store: store,
		now:   time.Now(),
		out:   &bytes.Buffer{},
	}

	origLauncher, origMachine := plat.Launcher, plat.Machine
	t.Cleanup(func() { plat.Launcher, plat.Machine = origLauncher, origMachine })
	plat.Machine = tableMachine{plat.Machine}
	var l *agyTestLauncher
	l = &agyTestLauncher{
		onStart: func(u platform.Unit) {
			go func() {
				var startID string
				for _, e := range u.Env {
					if v, ok := strings.CutPrefix(e, agy.EnvAgent+"="); ok {
						startID = v
					}
				}
				inbox := agy.InboxPath(dir, startID)
				logPath := agy.StreamPath(dir, startID)
				f, err := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // test FIFO
				if err != nil {
					return
				}
				_, _ = bufio.NewReader(f).ReadString('\n')
				_ = f.Close()
				_ = os.WriteFile(logPath, []byte(`{"event":"result","result":{"status":"error","error":"auth expired"}}`+"\n"), 0o600)
				l.mu.Lock()
				l.states[u.Name] = "failed"
				l.mu.Unlock()
			}()
		},
	}
	plat.Launcher = l

	sp := agentStart{
		name:  testAgyWorker,
		brief: "brief text",
		task:  testAgyTask,
		model: model,
		dir:   dir,
	}
	err = a.startAgyAgent(context.Background(), sp)
	if err == nil || !strings.Contains(err.Error(), "auth expired") || !strings.Contains(err.Error(), "start it again with agents start --harness agy") {
		t.Fatalf("startAgyAgent err = %v, want auth expired and restart hint", err)
	}
	if len(l.stopped) != 1 {
		t.Errorf("stopped = %v, want unit stopped", l.stopped)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "agy", "*")); len(matches) != 0 {
		t.Errorf("leftover agy files after failed start: %v", matches)
	}
	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 1 || st.Agents[0].Undelivered == "" {
		t.Fatalf("roster = %+v, want Undelivered recorded", st.Agents)
	}
}

func TestDryRunAgyStartAndEndAgy(t *testing.T) {
	const (
		startID = "33445566-7788-4999-aaaa-bbccddeeff00"
		model   = "gemini-3-1-pro-medium"
	)
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{cfg: &config.Config{StateDir: dir}, store: store, now: time.Now(), out: &out}

	sp := agentStart{
		name:  testAgyWorker,
		brief: "brief body",
		task:  "task",
		model: model,
		dir:   dir,
	}
	if err := a.dryRunAgyStart(sp, nil); err != nil {
		t.Fatalf("dryRunAgyStart: %v", err)
	}
	if !strings.Contains(out.String(), "as an agy agent on "+model) {
		t.Fatalf("dryRunAgyStart output = %q", out.String())
	}

	origLauncher := plat.Launcher
	t.Cleanup(func() { plat.Launcher = origLauncher })
	unit := agyUnit(startID)
	l := &agyTestLauncher{states: map[string]string{unit: unitActive}}
	plat.Launcher = l

	inbox := agy.InboxPath(dir, startID)
	if err := agy.MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agy.StreamPath(dir, startID), []byte("log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agy.AgentConvPath(dir, startID), []byte("conv-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := a.endAgy(context.Background(), startID)
	if !strings.Contains(got, "stopped its agy unit "+unit) || !strings.Contains(got, "removed its agy inbox") {
		t.Fatalf("endAgy = %q", got)
	}
	if got2 := a.endAgy(context.Background(), startID); got2 != "an agy agent has no desktop session" {
		t.Fatalf("second endAgy = %q", got2)
	}
}

func TestAgyHarnessDispatchInViewsAndHandover(t *testing.T) {
	const startID = "44556677-8899-4aaa-bbbb-ccddeeff0011"
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{StateDir: dir}, store: store, now: time.Now(), out: &bytes.Buffer{}}
	party := state.Party{HostSession: agy.HostPrefix + startID, Name: testAgyWorker}
	st := &state.State{
		Agents: []state.Agent{{Party: party, Task: testAgyTask}},
	}
	if host, why := a.archivableRecord(st, party); host != "" || why != "an agy agent has no desktop session" {
		t.Errorf("archivableRecord = (%q, %q)", host, why)
	}
	sv := &sessionView{Session: &claude.Session{Harness: agy.Harness, State: agy.StateBusy}}
	if got := roleText(sv, "agent: fix it"); got != "agy busy agent: fix it" {
		t.Errorf("roleText = %q", got)
	}
	if err := store.Update(func(s *state.State) ([]state.Event, error) {
		s.Agents = st.Agents
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.readHandover(testAgyWorker); err == nil || !strings.Contains(err.Error(), "is an agy agent") {
		t.Errorf("readHandover = %v, want refusal for agy agents", err)
	}
}

type agyProcMachine struct {
	platform.Machine
	procs map[int]*proc.Process
}

func (m agyProcMachine) Processes() (*proc.Table, error) {
	return &proc.Table{ByPID: m.procs}, nil
}

func TestNotifyAgyRemoteDaemonAndStopUnits(t *testing.T) {
	const (
		convID    = "conv-remote-1122"
		otherConv = "88888888-9999-4aaa-bbbb-cccccccccccc"
		daemon    = "antigravity-cli-daemon"
		model     = "claude-opus-4-6-thinking"
	)
	writeFakeAgy(t, "Available models:\n  "+model+"\n")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sup-1")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", "supervisor")

	dir := t.TempDir()
	brainDir := filepath.Join(dir, "brain")
	convDir := filepath.Join(dir, "conversations")
	if err := os.MkdirAll(convDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(convDir, convID+".db"), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	var restarted []string
	origRestart := restartAgyRemoteDaemon
	t.Cleanup(func() { restartAgyRemoteDaemon = origRestart })
	restartAgyRemoteDaemon = func(_ context.Context, u string) error {
		restarted = append(restarted, u)
		return nil
	}

	origLauncher, origMachine := plat.Launcher, plat.Machine
	t.Cleanup(func() { plat.Launcher, plat.Machine = origLauncher, origMachine })
	plat.Machine = tableMachine{plat.Machine}
	l := &agyTestLauncher{
		onStart: func(u platform.Unit) {
			go func() {
				var startID string
				for _, e := range u.Env {
					if v, ok := strings.CutPrefix(e, agy.EnvAgent+"="); ok {
						startID = v
					}
				}
				inbox := agy.InboxPath(dir, startID)
				logPath := agy.StreamPath(dir, startID)
				f, oerr := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // test FIFO
				if oerr != nil {
					return
				}
				defer func() { _ = f.Close() }()
				_, _ = bufio.NewReader(f).ReadString('\n')
				_, _ = agy.Drain(inbox)
				stream := `{"event":"init","conversation_id":"` + convID + `"}` + "\n" +
					`{"event":"step_update","step_update":{"conversation_id":"` + convID + `","step_type":"user_input"}}` + "\n"
				_ = os.WriteFile(logPath, []byte(stream), 0o600)
			}()
		},
	}
	plat.Launcher = l

	// 1. RemoteControl == false does not restart RemoteDaemon even when RemoteDaemon != "".
	var out bytes.Buffer
	a := &app{
		cfg: &config.Config{
			StateDir: dir,
			Agy: config.Agy{
				BrainDir:         brainDir,
				ConversationsDir: convDir,
				RemoteControl:    false,
				RemoteDaemon:     daemon,
			},
		},
		store: store,
		now:   time.Now(),
		out:   &out,
	}
	if err := a.startAgyAgent(context.Background(), agentStart{
		name:  "worker-no-rc",
		brief: "brief 1",
		task:  testAgyTask,
		model: model,
		dir:   dir,
	}); err != nil {
		t.Fatalf("startAgyAgent (RemoteControl=false): %v", err)
	}
	if len(restarted) != 0 {
		t.Fatalf("restarted = %v, want empty when RemoteControl is false", restarted)
	}

	// 2. RemoteControl == true restarts RemoteDaemon when no other agy session is busy and reports it.
	a.cfg.Agy.RemoteControl = true
	out.Reset()
	if err := a.startAgyAgent(context.Background(), agentStart{
		name:  "worker-rc",
		brief: "brief 2",
		task:  testAgyTask,
		model: model,
		dir:   dir,
	}); err != nil {
		t.Fatalf("startAgyAgent (RemoteControl=true): %v", err)
	}
	if !slices.Equal(restarted, []string{daemon}) {
		t.Fatalf("restarted = %v, want [%s]", restarted, daemon)
	}
	if !strings.Contains(out.String(), "restarted remote daemon "+daemon+" for conversation "+convID) {
		t.Fatalf("output missing restart report:\n%s", out.String())
	}

	// 3. Another running agy session in StateBusy prevents restarting RemoteDaemon and reports why.
	restarted = nil
	now := time.Now().UTC()
	otherTranscript := agy.TranscriptPath(brainDir, otherConv)
	if err := os.MkdirAll(filepath.Dir(otherTranscript), 0o750); err != nil {
		t.Fatal(err)
	}
	busyStep := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"` + now.Format(time.RFC3339) + `","content":"busy turn"}` + "\n"
	if err := os.WriteFile(otherTranscript, []byte(busyStep), 0o600); err != nil {
		t.Fatal(err)
	}
	plat.Machine = agyProcMachine{
		Machine: origMachine,
		procs: map[int]*proc.Process{
			4242: {PID: 4242, Comm: agy.Comm, Args: []string{agy.Comm, "--conversation", otherConv}, Start: now.Add(-time.Minute)},
		},
	}
	a.now = now
	gotLine := a.notifyAgyRemoteDaemon(context.Background(), "new-start-id", convID)
	if len(restarted) != 0 {
		t.Fatalf("restarted = %v, want skip while another agy session is busy", restarted)
	}
	if !strings.Contains(gotLine, "not restarted: agy session") || !strings.Contains(gotLine, "is busy mid-turn") {
		t.Fatalf("notifyAgyRemoteDaemon = %q, want busy skip message", gotLine)
	}

	// 4. turnPatterns includes beekeeper-agy-* and beekeeper-omp-*, and sparedUnit matches spared agy/omp sessions.
	if !slices.Contains(turnPatterns, "beekeeper-agy-*") || !slices.Contains(turnPatterns, "beekeeper-omp-*") {
		t.Errorf("turnPatterns = %v, want beekeeper-agy-* and beekeeper-omp-*", turnPatterns)
	}
	spare := []state.Party{
		{HostSession: agy.HostPrefix + "11223344-5566-7788-99aa-bbccddeeff00", Name: "spared-agy"},
		{HostSession: omp.HostPrefix + "99887766-5544-3322-1100-ffeeddccbbaa", Name: "spared-omp"},
	}
	if !sparedUnit("beekeeper-agy-11223344.service", "", spare) {
		t.Error("sparedUnit did not spare spared agy unit")
	}
	if !sparedUnit("beekeeper-omp-99887766.service", "", spare) {
		t.Error("sparedUnit did not spare spared omp unit")
	}
	if sparedUnit("beekeeper-agy-deadbeef.service", "", spare) {
		t.Error("sparedUnit spared non-spared agy unit")
	}

	// 5. A start refused by recordStart (live session already registered under the name) creates no orphan FIFO in <stateDir>/agy.
	refusedDir := t.TempDir()
	refusedStore, err := state.Open(refusedDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := refusedStore.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Session: otherConv, Name: "w"}, Task: testAgyTask}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	aRefused := &app{
		cfg: &config.Config{
			StateDir: refusedDir,
			Agy:      config.Agy{BrainDir: brainDir},
		},
		store: refusedStore,
		now:   now,
		out:   &bytes.Buffer{},
	}
	if err := aRefused.startAgyAgent(context.Background(), agentStart{name: "w", brief: "b", model: model, dir: refusedDir}); err == nil {
		t.Fatal("expected startAgyAgent on live duplicate agent name to fail")
	}
	if matches, _ := filepath.Glob(filepath.Join(refusedDir, "agy", "*")); len(matches) != 0 {
		t.Errorf("orphan agy files after refused recordStart: %v", matches)
	}

	// 6. board next --claim allows an agy caller (HostSession set, Session empty) past the --as check.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", "")
	t.Setenv(agy.EnvAgent, "11223344-5566-7788-99aa-bbccddeeff00")
	t.Setenv(agy.EnvName, testAgyWorker)
	bnCmd := a.boardNextCmd()
	bnCmd.SilenceErrors = true
	bnCmd.SilenceUsage = true
	bnCmd.SetArgs([]string{"--claim"})
	if berr := bnCmd.Execute(); berr != nil && strings.Contains(berr.Error(), "run it inside a Claude Code session") {
		t.Errorf("board next --claim rejected agy session caller: %v", berr)
	}
}
