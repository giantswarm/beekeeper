//go:build unix

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
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
	if err := a.wakeCodex(by, ag, id, "mid-turn wake message"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "written to its inbox") {
		t.Fatalf("output %q", out.String())
	}

	// Verify `beekeeper hook pretooluse --format codex` drains the queued wake
	// message and prints additionalContext.
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
	var parsed struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(hookOut.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal hook output %q: %v", hookOut.String(), err)
	}
	if parsed.HookSpecificOutput.HookEventName != "PreToolUse" || parsed.HookSpecificOutput.AdditionalContext != "mid-turn wake message" {
		t.Errorf("unexpected hook output: %+v", parsed)
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
	hl := &haltLauncher{installed: map[string]string{}}
	plat.Launcher = hl
	defer func() { plat.Launcher = oldLauncher }()

	oldSend := sendCodexWhenReady
	var deliveredInbox, deliveredMsg string
	sendCodexWhenReady = func(_ context.Context, inbox, msg string, _ time.Duration) error {
		deliveredInbox, deliveredMsg = inbox, msg
		return nil
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

	if len(hl.started) != 1 {
		t.Fatalf("launched units = %d, want 1", len(hl.started))
	}
	u := hl.started[0]
	if u.Name != "beekeeper-codex-abcdef12" {
		t.Errorf("unit name = %q, want beekeeper-codex-abcdef12", u.Name)
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
	if err := os.WriteFile(cfgPath, []byte("stateDir: "+stateDir+"\nleaseDir: "+leaseDir+"\n"), 0o600); err != nil {
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
