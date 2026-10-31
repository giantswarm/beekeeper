//go:build unix

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestWakeCodexAndPreToolUseHook(t *testing.T) {
	const id = "1234abcd-0000-4000-8000-000000000000"
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{cfg: &config.Config{StateDir: dir}, store: store, out: &out}
	ag := state.Agent{Party: state.Party{HostSession: codex.HostPrefix + id, Name: "test: codex"}}
	by := state.Party{Name: "test"}
	inbox := codex.InboxPath(dir, id)
	if err := codex.MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	if err := a.wakeCodex(by, ag, id, "hello"); err == nil || !strings.Contains(err.Error(), "no longer runs") {
		t.Fatalf("wake with no reader = %v, want refusal", err)
	}

	r, err := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // test inbox
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	if err := codex.RecordThread(dir, inbox, "thread-wake-1"); err != nil {
		t.Fatal(err)
	}
	if err := a.wakeCodex(by, ag, id, "next-turn wake message"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "written to its inbox, it runs the message as its next turn") {
		t.Fatalf("output %q", out.String())
	}

	// Verify `beekeeper hook pretooluse --format codex` does not steal or
	// duplicate the queued wake message mid-turn, leaving it queued for Drain.
	cfgFile := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("stateDir: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hookCmd := New()
	var hookOut bytes.Buffer
	hookCmd.SetOut(&hookOut)
	hookCmd.SetIn(strings.NewReader(`{"session_id":"thread-wake-1","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"git status"}}`))
	hookCmd.SetArgs(append([]string{"--" + configFlag + "=" + cfgFile}, strings.Fields("hook pretooluse --format="+codex.Harness)...))
	if err := hookCmd.Execute(); err != nil {
		t.Fatalf("hook pretooluse: %v", err)
	}

	msgs, err := codex.Drain(inbox)
	if err != nil || len(msgs) != 1 || msgs[0] != "next-turn wake message" {
		t.Fatalf("Drain(inbox) = (%v, %v), want [next-turn wake message]", msgs, err)
	}
}

func TestStartCodexAgentAndIdentity(t *testing.T) {
	const (
		testCodexModel = "gpt-5.7"
		testIssueRef   = "giantswarm/beekeeper#1708"
		testWorkerName = "codex-worker"
		startID        = "abcdef12-3456-4000-8000-000000000000"
	)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	leaseDir := filepath.Join(root, "leases")
	userCodex := filepath.Join(root, "user-codex")
	if err := os.MkdirAll(userCodex, 0o700); err != nil {
		t.Fatal(err)
	}
	authFile := filepath.Join(userCodex, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"access_token":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	userConfig := filepath.Join(userCodex, "config.toml")
	if err := os.WriteFile(userConfig, []byte("[features]\nplugins = true\n\n[projects.\""+root+"\"]\ntrust_level = \"trusted\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeBinDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(fakeBinDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBinDir, codex.Comm), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // test executable script
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	oldLauncher := plat.Launcher
	oldMachine := plat.Machine
	hl := &haltLauncher{installed: map[string]string{}}
	plat.Launcher = hl
	// The gate reads the machine's guards for real, so the test gives it a
	// roomy one: memory and load with room to spare, whatever the host
	// running the test carries. testMachine rides every platform reload
	// below: a config-driven start rebuilds the platform, and the stub's
	// Mem is not available on every build.
	testMachine = roomyMachine{plat.Machine}
	plat.Machine = testMachine
	t.Cleanup(func() {
		testMachine = nil
		plat.Launcher = oldLauncher
		plat.Machine = oldMachine
	})

	oldSend := sendCodexWhenReady
	var deliveredInbox, deliveredMsg string
	sendCodexWhenReady = func(_ context.Context, inbox, msg string, _ time.Duration) error {
		deliveredInbox, deliveredMsg = inbox, msg
		logPath := strings.TrimSuffix(inbox, ".in") + ".jsonl"
		return os.WriteFile(logPath, []byte("{\"type\":\"thread.started\",\"thread_id\":\"thread-1234\"}\n{\"type\":\"turn.started\"}\n"), 0o600)
	}
	defer func() { sendCodexWhenReady = oldSend }()

	store, err := state.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		StateDir: stateDir,
		LeaseDir: leaseDir,
		Codex: config.Codex{
			SessionsDir: filepath.Join(userCodex, "sessions"),
			AuthFile:    authFile,
			ConfigFile:  userConfig,
			Model:       testCodexModel,
		},
	}
	var out bytes.Buffer
	a := &app{
		cfg:   cfg,
		store: store,
		now:   time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC),
		out:   &out,
		as:    supervisorName,
	}

	projectRules := filepath.Join(root, ".claude", "rules")
	if err := os.MkdirAll(projectRules, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRules, "merge.md"), []byte("project-specific squash rule"), 0o600); err != nil {
		t.Fatal(err)
	}

	sp := agentStart{
		id:     startID,
		name:   "beekeeper#1708: codex",
		role:   workerRole,
		brief:  "Worker rules\n\nTask:\nWork https://github.com/giantswarm/beekeeper/issues/1708",
		task:   "Work https://github.com/giantswarm/beekeeper/issues/1708",
		serves: testIssueRef,
		dir:    root,
		model:  testCodexModel,
	}
	if err := a.startCodexAgent(context.Background(), sp); err != nil {
		t.Fatalf("startCodexAgent: %v", err)
	}

	if !strings.Contains(deliveredMsg, "project-specific squash rule") || !strings.HasSuffix(deliveredMsg, sp.brief) {
		t.Fatalf("first inbox message lacks project rules before task: %q", deliveredMsg)
	}

	if len(hl.started) != 1 {
		t.Fatalf("launched units = %d, want 1", len(hl.started))
	}
	u := hl.started[0]
	if u.Name != "beekeeper-codex-abcdef12" {
		t.Errorf("unit name = %q, want beekeeper-codex-abcdef12", u.Name)
	}
	if u.KeepChildren {
		t.Errorf("unit KeepChildren = true, want false so stopping the unit terminates child turns")
	}
	wantCodexHome := codex.HomeDir(stateDir)
	for _, wantEnv := range []string{
		codex.EnvAgent + "=" + startID,
		codex.EnvName + "=beekeeper#1708: codex",
		"CODEX_HOME=" + wantCodexHome,
		"ZDOTDIR=" + codex.ZshDir(wantCodexHome),
	} {
		if !slices.Contains(u.Env, wantEnv) {
			t.Errorf("unit env %v missing %q", u.Env, wantEnv)
		}
	}
	if deliveredInbox != codex.InboxPath(stateDir, startID) || !strings.Contains(deliveredMsg, "Worker rules") {
		t.Errorf("delivered (%q, %q)", deliveredInbox, deliveredMsg)
	}

	// Verify isolated CODEX_HOME files were generated.
	if _, err := os.Stat(filepath.Join(wantCodexHome, "config.toml")); err != nil {
		t.Errorf("missing isolated config.toml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wantCodexHome, "hooks.json")); err != nil {
		t.Errorf("missing isolated hooks.json: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(wantCodexHome, "auth.json")); err != nil || target != authFile {
		t.Errorf("auth.json symlink = %q (%v), want %q", target, err, authFile)
	}

	// Verify the codex agent is on the roster and its identity environment
	// works for caller(), park, log add, note add, and idle --done.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "other-claude-session")
	t.Setenv(codex.EnvAgent, startID)
	t.Setenv(codex.EnvName, "beekeeper#1708: codex")

	aWorker := &app{
		cfg:   cfg,
		store: store,
		now:   time.Date(2026, 10, 28, 12, 5, 0, 0, time.UTC),
		out:   &bytes.Buffer{},
	}
	me, err := aWorker.caller()
	if err != nil {
		t.Fatalf("caller: %v", err)
	}
	if me.HostSession != codex.HostPrefix+startID || me.Name != "beekeeper#1708: codex" {
		t.Fatalf("caller = %+v", me)
	}

	var dryOut bytes.Buffer
	aDry := &app{
		cfg:   cfg,
		store: store,
		now:   time.Date(2026, 10, 28, 12, 5, 0, 0, time.UTC),
		out:   &dryOut,
	}
	if err := aDry.dryRunCodexStart(agentStart{
		name:  testWorkerName,
		brief: "Work https://github.com/giantswarm/beekeeper/issues/1708",
		dir:   root,
		model: testCodexModel,
	}, nil); err != nil {
		t.Fatalf("dryRunCodexStart: %v", err)
	}
	if !strings.Contains(dryOut.String(), "as a codex agent on "+testCodexModel) {
		t.Errorf("dryRunCodexStart output = %q", dryOut.String())
	}

	// Test --dry-run CLI argument parsing with both --name <name> <brief-file>
	// and positional <name> <brief-file>.
	briefPath := filepath.Join(root, "brief.md")
	if err := os.WriteFile(briefPath, []byte("Work https://github.com/giantswarm/beekeeper/issues/1708\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "config.yaml")
	// The dry run's gate reads the machine's guards for real (its own
	// review's demand), so the config carries a memory floor the roomy
	// machine of the launcher stub above clears: the guard weighs on the
	// dry run as on a real start, here one that fits.
	if err := os.WriteFile(cfgPath, []byte("stateDir: "+stateDir+"\nleaseDir: "+leaseDir+"\ncapacity:\n  availMinMiB: 1024\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--" + configFlag + "=" + cfgPath, agentsName, agentStartName, "--dry-run=true", testHarnessFlag, codex.Harness, modelFlag, testCodexModel, nameFlag, testWorkerName, usageDirFlag, root, briefPath},
		{"--" + configFlag + "=" + cfgPath, agentsName, agentStartName, "--dry-run=true", testHarnessFlag, codex.Harness, modelFlag, testCodexModel, usageDirFlag, root, testWorkerName, briefPath},
	} {
		cmd := New()
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute(%v): %v", args, err)
		}
	}

	// Test park, log add, note add, and idle --done as the codex worker.
	for _, args := range [][]string{
		append(strings.Fields("--"+configFlag+"="+cfgPath+" log add"), "progress on https://github.com/giantswarm/beekeeper/issues/1708"),
		append(strings.Fields("--"+configFlag+"="+cfgPath+" "+agentsName+" park --on "+testIssueRef), "desk review of https://github.com/giantswarm/beekeeper/issues/1708"),
		append(strings.Fields("--"+configFlag+"="+cfgPath+" note add --for "+supervisorName+" --ref "+testIssueRef+" --due 18:00 --default continue --status-quo running --why verify"), "Should we proceed with https://github.com/giantswarm/beekeeper/issues/1708?"),
		append(strings.Fields("--"+configFlag+"="+cfgPath+" "+agentsName+" "+stateIdle+" --done --problem "+ghNone+" --report"), "Completed https://github.com/giantswarm/beekeeper/issues/1708"),
	} {
		cmd := New()
		var cmdOut bytes.Buffer
		cmd.SetOut(&cmdOut)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute(%v) as codex worker: %v", args, err)
		}
	}

	// Verify standalone codex sessions (not started by beekeeper) are refused
	// by ui messageSession and archivableRecord.
	ownCodex := &claude.Session{Name: "codex in repo", Harness: codex.Harness}
	if _, err := aWorker.messageSession(context.Background(), []*claude.Session{ownCodex}, ownCodex, "hi"); err == nil || !strings.Contains(err.Error(), "beekeeper did not start") {
		t.Errorf("messageSession on standalone codex = %v, want refusal", err)
	}
}

func TestStartCodexValidationCleanupAndStop(t *testing.T) {
	const (
		testCodexModel = "gpt-5.7"
		startID        = "99999999-0000-4000-8000-000000000000"
	)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	leaseDir := filepath.Join(root, "leases")
	fakeBinDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(fakeBinDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBinDir, codex.Comm), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	oldLauncher := plat.Launcher
	oldMachine := plat.Machine
	hl := &haltLauncher{installed: map[string]string{}}
	plat.Launcher = hl
	// The gate reads the machine's guards for real: a roomy machine keeps
	// this test about the identity, not the memory reading. testMachine
	// rides the reloads of the config-driven starts below.
	testMachine = roomyMachine{plat.Machine}
	plat.Machine = testMachine
	t.Cleanup(func() {
		testMachine = nil
		plat.Launcher = oldLauncher
		plat.Machine = oldMachine
	})

	store, err := state.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		StateDir: stateDir,
		LeaseDir: leaseDir,
		Codex: config.Codex{
			SessionsDir: filepath.Join(root, "sessions"),
			AuthFile:    filepath.Join(root, "auth.json"),
			ConfigFile:  filepath.Join(root, "config.toml"),
			Model:       testCodexModel,
		},
	}
	a := &app{
		cfg:   cfg,
		store: store,
		now:   time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC),
		out:   &bytes.Buffer{},
		as:    supervisorName,
	}

	// 1. Refuse when beekeeper is stopped.
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Stop = &state.Stop{Reason: "hold all", By: state.Party{Name: holderPerson}, At: a.now}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.startCodexAgent(context.Background(), agentStart{name: "stopped-codex", dir: root, model: testCodexModel}); err == nil || !strings.Contains(err.Error(), "beekeeper is stopped") {
		t.Fatalf("startCodexAgent while stopped = %v, want stopped refusal", err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Stop = nil
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	// 2. Refuse when --dir is not a directory.
	notDir := filepath.Join(root, "not-a-dir")
	if err := a.startCodexAgent(context.Background(), agentStart{name: "bad-dir", dir: notDir, model: testCodexModel}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("startCodexAgent with bad dir = %v, want not a directory error", err)
	}

	// 3. Clean up unit and inbox/log files when first turn fails to start.
	oldSend := sendCodexWhenReady
	sendCodexWhenReady = func(_ context.Context, inbox, _ string, _ time.Duration) error {
		logPath := strings.TrimSuffix(inbox, ".in") + ".jsonl"
		return os.WriteFile(logPath, []byte("{\"type\":\"thread.started\",\"thread_id\":\"thread-fail\"}\n{\"type\":\"error\",\"message\":\"api failure\"}\n"), 0o600)
	}
	defer func() { sendCodexWhenReady = oldSend }()

	err = a.startCodexAgent(context.Background(), agentStart{
		id:    startID,
		name:  "failing-codex",
		brief: "Task brief",
		task:  "Task brief",
		dir:   root,
		model: testCodexModel,
	})
	if err == nil || !strings.Contains(err.Error(), "api failure") {
		t.Fatalf("startCodexAgent on turn error = %v, want api failure", err)
	}
	if !slices.Contains(hl.stopped, codexUnit(startID)) {
		t.Errorf("hl.stopped = %v, want %s stopped on failure", hl.stopped, codexUnit(startID))
	}
	if _, statErr := os.Stat(codex.InboxPath(stateDir, startID)); !os.IsNotExist(statErr) {
		t.Errorf("expected inbox to be removed on failure, statErr = %v", statErr)
	}
	if _, statErr := os.Stat(codex.LogPath(stateDir, startID)); !os.IsNotExist(statErr) {
		t.Errorf("expected logPath to be removed on failure, statErr = %v", statErr)
	}

	// 4. Verify turnPatterns, sparedUnit, and callerEnv include codex.
	if !slices.Contains(turnPatterns, "beekeeper-codex-*") {
		t.Errorf("turnPatterns %v missing beekeeper-codex-*", turnPatterns)
	}
	sparedParty := []state.Party{{HostSession: codex.HostPrefix + startID}}
	if !sparedUnit(codexUnit(startID), "", sparedParty) {
		t.Errorf("sparedUnit should spare %s", codexUnit(startID))
	}
	if sparedUnit(codexUnit("other1234"), "", sparedParty) {
		t.Errorf("sparedUnit should not spare non-spared codex unit")
	}
	for _, want := range []string{codex.EnvAgent, codex.EnvName} {
		if !slices.Contains(callerEnv, want) {
			t.Errorf("callerEnv %v missing %q", callerEnv, want)
		}
	}
}

func TestStopRunnerSubprocessStopsChildOnSIGTERM(t *testing.T) {
	if os.Getenv("BEEKEEPER_TEST_CODEX_RUNNER") == "1" {
		if i := slices.Index(os.Args, "--"); i >= 0 {
			cmd := New()
			cmd.SetArgs(os.Args[i+1:])
			if err := cmd.Execute(); err != nil {
				os.Exit(1)
			}
			os.Exit(0)
		}
	}

	stateDir := t.TempDir()
	const id = "runner-sigterm-test"
	inbox := codex.InboxPath(stateDir, id)
	logPath := codex.LogPath(stateDir, id)
	childPIDFile := filepath.Join(stateDir, "child.pid")

	fakeCodex := filepath.Join(stateDir, "fake-codex.sh")
	script := `#!/usr/bin/env bash
set -euo pipefail
cat >/dev/null
echo '{"type":"thread.started","thread_id":"thread-sigterm"}'
echo '{"type":"turn.started"}'
sleep 60 &
child=$!
echo "$child" > "` + childPIDFile + `"
wait "$child"
`
	if err := os.WriteFile(fakeCodex, []byte(script), 0o700); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner := exec.Command(self, "-test.run=^TestStopRunnerSubprocessStopsChildOnSIGTERM$", "--", codexRunCmdName, inbox, logPath, fakeCodex, "gpt-5.7") //nolint:gosec // test binary
	runner.Env = append(os.Environ(), "BEEKEEPER_TEST_CODEX_RUNNER=1")
	if err := runner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if runner.Process != nil {
			_ = runner.Process.Kill()
		}
	}()

	// Wait until runner opens the inbox and start an active turn.
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := codex.Send(inbox, "start turn")
		if err == nil {
			break
		}
		if !errors.Is(err, codex.ErrNotRunning) || time.Now().After(deadline) {
			t.Fatalf("Send: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	var childPID int
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(childPIDFile) //nolint:gosec // test file
		if err == nil && len(bytes.TrimSpace(b)) > 0 {
			if err := json.Unmarshal(bytes.TrimSpace(b), &childPID); err == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID <= 0 {
		t.Fatal("child PID was not written")
	}
	defer func() {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
	}()

	// Send SIGTERM to the codex-run process, simulating unit stop / endCodex.
	if err := runner.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM runner: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- runner.Wait() }()
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not exit within 3s after SIGTERM")
	}

	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child process %d remained alive after SIGTERM to runner (kill(0) = %v)", childPID, err)
	}
}

func TestFailedStartAndEndCodexStopActiveChildTurn(t *testing.T) {
	if os.Getenv("BEEKEEPER_TEST_CODEX_RUNNER_UNIT") == "1" {
		if i := slices.Index(os.Args, "--"); i >= 0 {
			cmd := New()
			cmd.SetArgs(os.Args[i+1:])
			if err := cmd.Execute(); err != nil {
				os.Exit(1)
			}
			os.Exit(0)
		}
	}

	const (
		testCodexModel = "gpt-5.7"
		startID        = "88888888-0000-4000-8000-000000000000"
	)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	leaseDir := filepath.Join(root, "leases")
	childPIDFile := filepath.Join(root, "child.pid")
	fakeBinDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(fakeBinDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeCodex := filepath.Join(fakeBinDir, codex.Comm)
	script := `#!/usr/bin/env bash
set -euo pipefail
cat >/dev/null
echo '{"type":"thread.started","thread_id":"thread-active-child"}'
echo '{"type":"turn.started"}'
sleep 60 &
child=$!
echo "$child" > "` + childPIDFile + `"
wait "$child"
`
	if err := os.WriteFile(fakeCodex, []byte(script), 0o700); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	inbox := codex.InboxPath(stateDir, startID)
	logPath := codex.LogPath(stateDir, startID)
	runner := exec.Command(self, "-test.run=^TestFailedStartAndEndCodexStopActiveChildTurn$", "--", codexRunCmdName, inbox, logPath, fakeCodex, testCodexModel) //nolint:gosec // test binary
	runner.Env = append(os.Environ(), "BEEKEEPER_TEST_CODEX_RUNNER_UNIT=1")
	if err := runner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if runner.Process != nil {
			_ = runner.Process.Kill()
		}
	}()

	// Wait until the runner opens the inbox and start a turn with an active child.
	if err := sendCodexWhenReady(context.Background(), inbox, "start turn", 3*time.Second); err != nil {
		t.Fatalf("sendCodexWhenReady: %v", err)
	}
	var childPID int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b, rerr := os.ReadFile(childPIDFile) //nolint:gosec // test file
		if rerr == nil && len(bytes.TrimSpace(b)) > 0 {
			if json.Unmarshal(bytes.TrimSpace(b), &childPID) == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID <= 0 {
		t.Fatal("child PID was not written")
	}
	defer func() { _ = syscall.Kill(childPID, syscall.SIGKILL) }()

	// Wire haltLauncher so Stop sends SIGTERM to the running unit's runner and waits for it.
	oldLauncher := plat.Launcher
	unitName := codexUnit(startID)
	hl := &haltLauncher{
		installed: map[string]string{unitName: holdActive},
	}
	plat.Launcher = stopExecLauncher{
		Launcher: hl,
		stopFn: func(_ context.Context, name string) error {
			if name == unitName && runner.Process != nil {
				_ = runner.Process.Signal(syscall.SIGTERM)
				_ = runner.Wait()
			}
			return hl.Stop(context.Background(), name)
		},
	}
	defer func() { plat.Launcher = oldLauncher }()

	store, err := state.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{
		cfg:   &config.Config{StateDir: stateDir, LeaseDir: leaseDir},
		store: store,
		out:   &bytes.Buffer{},
	}
	summary := a.endCodex(context.Background(), startID)
	if !strings.Contains(summary, "stopped its codex unit") || !strings.Contains(summary, "removed its codex inbox") {
		t.Fatalf("endCodex = %q", summary)
	}
	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child process %d remained alive after endCodex (kill(0) = %v)", childPID, err)
	}
	if _, statErr := os.Stat(inbox); !os.IsNotExist(statErr) {
		t.Errorf("expected inbox removed after endCodex, got %v", statErr)
	}
}

type stopExecLauncher struct {
	platform.Launcher
	stopFn func(context.Context, string) error
}

func (s stopExecLauncher) Stop(ctx context.Context, name string) error {
	return s.stopFn(ctx, name)
}

func TestBackgroundDropsCodexIdentity(t *testing.T) {
	parentEnv := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=/home/tester",
		codex.EnvAgent + "=parent-codex-id",
		codex.EnvName + "=parent-codex-worker",
		"CODEX_HOME=/tmp/state/codex/home",
		"ZDOTDIR=/tmp/state/codex/home/zsh",
		agy.EnvAgent + "=parent-agy-id",
		omp.EnvAgent + "=parent-omp-id",
		omp.EnvCredentials + "=OPENAI_API_KEY",
		"SHELL=/tmp/state/omp/bin/bash",
		"CLAUDE_CODE_SESSION_ID=parent-claude",
	}

	cleaned := cleanEnv(parentEnv, "/tmp/config.yaml")
	childEnv := append(slices.Clone(cleaned),
		"CLAUDE_CODE_SESSION_ID=child-claude-id",
		"CLAUDE_CODE_SESSION_NAME=child-claude-worker",
	)
	lookup := func(env []string) func(string) string {
		m := make(map[string]string, len(env))
		for _, kv := range env {
			k, v, ok := strings.Cut(kv, "=")
			if ok {
				m[k] = v
			}
		}
		return func(k string) string { return m[k] }
	}

	got := envParty(lookup(childEnv))
	want := state.Party{Session: "child-claude-id", Name: "child-claude-worker"}
	if got != want {
		t.Fatalf("envParty(cleanEnv) = %+v, want %+v (cleaned env: %v)", got, want, cleaned)
	}
	for _, forbidden := range []string{codex.EnvAgent, codex.EnvName, "CODEX_HOME", "ZDOTDIR", agy.EnvAgent, omp.EnvAgent, omp.EnvCredentials, "SHELL"} {
		if v := lookup(cleaned)(forbidden); v != "" {
			t.Errorf("cleanEnv kept %s=%q", forbidden, v)
		}
	}

	browsed := browseEnv(parentEnv)
	gotBrowse := envParty(lookup(append(slices.Clone(browsed),
		"CLAUDE_CODE_SESSION_ID=browse-turn-id",
		"CLAUDE_CODE_SESSION_NAME=beekeeper browse",
	)))
	wantBrowse := state.Party{Session: "browse-turn-id", Name: "beekeeper browse"}
	if gotBrowse != wantBrowse {
		t.Fatalf("envParty(browseEnv) = %+v, want %+v", gotBrowse, wantBrowse)
	}
}
