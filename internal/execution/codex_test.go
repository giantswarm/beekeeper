package execution

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	fixtureCodexVersion = "0.166.0"
	fixtureThread       = "019a0000-0000-7000-8000-000000000001"
)

// codexScript is a fake codex CLI: version and login probes, a first turn
// that creates fixtureThread, and resumed turns that start on the thread they
// were given (or on a foreign one when BEEKEEPER_FAKE_FOREIGN names it). Each
// call's arguments and prompt are appended to calls for the test to read.
const codexScript = `#!/bin/sh
set -eu
calls="$(dirname "$0")/calls"
if [ "$1" = --version ]; then echo "codex-cli ${BEEKEEPER_FAKE_VERSION:-0.166.0}"; exit 0; fi
if [ "$1" = login ]; then
  if [ -e "$(dirname "$0")/logged-out" ]; then echo "Not logged in" >&2; exit 1; fi
  echo "Logged in using ChatGPT" >&2; exit 0
fi
test -n "${CODEX_HOME}"
test -z "${OPENAI_API_KEY+x}"
test -z "${CLAUDE_CONFIG_DIR+x}"
test -n "${BEEKEEPER_EXECUTION_ID}"
test -n "${BEEKEEPER_CONFIG}"
input=$(cat)
printf '%s|%s|%s\n' "$CODEX_HOME" "$*" "$input" >> "$calls"
if [ -e "$(dirname "$0")/no-thread" ]; then echo '{"type":"error"}'; exit 1; fi
if [ "$2" = resume ]; then
  thread=""
  for a in "$@"; do case "$a" in 019a*) thread="$a";; esac; done
  if [ -e "$(dirname "$0")/foreign" ]; then thread=019a0000-0000-7000-8000-00000000ffff; fi
  printf '{"type":"thread.started","thread_id":"%s"}\n' "$thread"
else
  printf '{"type":"thread.started","thread_id":"%s"}\n' 019a0000-0000-7000-8000-000000000001
fi
echo '{"type":"turn.started"}'
if [ -e "$(dirname "$0")/fail-turn" ]; then exit 1; fi
if [ -e "$(dirname "$0")/turn-failed" ]; then echo '{"type":"turn.failed","error":{"message":"fixture"}}'; exit 0; fi
echo '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`

// codexFixture turns the testFixture host into a claude + codex host and the
// request into a codex request bound to the prepared policy.
func codexFixture(t *testing.T) (fixture, string) {
	t.Helper()
	f := testFixture(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte(codexScript), 0o700); err != nil { //nolint:gosec // executable fake harness in the test's own temporary directory
		t.Fatal(err)
	}
	f.s.Config.Harnesses[HarnessCodex] = HarnessConfig{Executable: bin, Version: fixtureCodexVersion, ConfigDir: filepath.Join(dir, "login")}
	digest, err := f.s.Prepare(f.s.Policy())
	if err != nil {
		t.Fatal(err)
	}
	f.r.Harness, f.r.Model, f.r.PolicyDigest = HarnessCodex, "gpt-fixture", digest
	return f, dir
}

func TestHostConfigHarnesses(t *testing.T) {
	f := testFixture(t)
	codex := HarnessConfig{Executable: "/usr/local/bin/codex", Version: fixtureCodexVersion, ConfigDir: "/srv/codex"}
	for name, test := range map[string]struct {
		harnesses map[string]HarnessConfig
		ok        bool
	}{
		"claude only":           {map[string]HarnessConfig{HarnessClaude: f.s.Config.Harnesses[HarnessClaude]}, true},
		"codex only":            {map[string]HarnessConfig{HarnessCodex: codex}, true},
		"claude and codex":      {map[string]HarnessConfig{HarnessClaude: f.s.Config.Harnesses[HarnessClaude], HarnessCodex: codex}, true},
		"none":                  {map[string]HarnessConfig{}, false},
		"agy has no adapter":    {map[string]HarnessConfig{"agy": codex}, false},
		"codex without login":   {map[string]HarnessConfig{HarnessCodex: {Executable: codex.Executable, Version: codex.Version}}, false},
		"codex below its range": {map[string]HarnessConfig{HarnessCodex: {Executable: codex.Executable, Version: "0.100.0", ConfigDir: codex.ConfigDir}}, false},
		"codex relative binary": {map[string]HarnessConfig{HarnessCodex: {Executable: "codex", Version: codex.Version, ConfigDir: codex.ConfigDir}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			c := f.s.Config
			c.Harnesses = test.harnesses
			if err := c.Validate(); (err == nil) != test.ok {
				t.Fatalf("validation = %v, want ok %v", err, test.ok)
			}
		})
	}
	for _, test := range []struct {
		harnesses []string
		ok        bool
	}{{[]string{HarnessClaude}, true}, {[]string{HarnessCodex}, true}, {[]string{HarnessClaude, HarnessCodex}, true}, {nil, false}, {[]string{HarnessOmp}, true}, {[]string{HarnessCodex, HarnessCodex}, false}} {
		h := Host{Endpoint: Endpoint{SSH: "host-a.example", Executable: "/usr/local/bin/beekeeper", Config: "/etc/beekeeper/worker.json"}, Identity: fixtureHost, WorkspaceRoot: "/srv/workspaces", Repositories: map[string]string{f.r.Repository: "/srv/source"}, Harnesses: test.harnesses, PolicyDigest: Digest([]byte("policy")), MaxBusy: 1}
		if err := h.Validate(); (err == nil) != test.ok {
			t.Fatalf("host harnesses %v: %v, want ok %v", test.harnesses, err, test.ok)
		}
	}
}

func TestPrepareInstallsManagedCodexHome(t *testing.T) {
	plain := testFixture(t)
	if plain.s.Policy().CodexDigest != "" {
		t.Fatal("a claude-only host policy binds a codex home")
	}
	f, dir := codexFixture(t)
	p, err := f.s.InstalledPolicy()
	if err != nil || p.CodexDigest == "" {
		t.Fatalf("codex policy not installed: %v", err)
	}
	home := f.s.codexHomeDir()
	hooks, err := os.ReadFile(filepath.Join(home, "hooks.json")) //nolint:gosec // test-owned managed home
	if err != nil || !strings.Contains(string(hooks), "agents execution-guard --format codex") || !strings.Contains(string(hooks), "PreToolUse") {
		t.Fatalf("codex hooks do not run the execution guard: %s %v", hooks, err)
	}
	config, err := os.ReadFile(filepath.Join(home, "config.toml")) //nolint:gosec // test-owned managed home
	if err != nil || !strings.Contains(string(config), "hooks = true") || !strings.Contains(string(config), "computer_use = false") || strings.Contains(string(config), "[projects") {
		t.Fatalf("managed codex configuration: %s %v", config, err)
	}
	if rules, err := os.ReadFile(filepath.Join(home, "AGENTS.md")); err != nil || string(rules) != string(f.s.Rules) { //nolint:gosec // test-owned managed home
		t.Fatal("codex home does not carry the embedded worker rules")
	}
	if target, err := os.Readlink(filepath.Join(home, "auth.json")); err != nil || target != filepath.Join(dir, "login", "auth.json") {
		t.Fatalf("codex login is not linked from the host's own CODEX_HOME: %q %v", target, err)
	}
	if err := os.WriteFile(filepath.Join(home, "hooks.json"), []byte(`{"hooks":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.InstalledPolicy(); err == nil {
		t.Fatal("tampered codex hooks still match the policy")
	}
}

func TestCheckAdvertisesOnlyPassingHarnesses(t *testing.T) {
	f, dir := codexFixture(t)
	ctx := context.Background()
	caps, err := f.s.Check(ctx)
	if err != nil || caps.Harnesses[HarnessCodex] != fixtureCodexVersion || caps.Harnesses[HarnessClaude] == "" || caps.Unavailable != nil {
		t.Fatalf("both harnesses should pass: %+v %v", caps, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logged-out"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	caps, err = f.s.Check(ctx)
	if err != nil || caps.Harnesses[HarnessCodex] != "" || caps.Harnesses[HarnessClaude] == "" || !strings.Contains(caps.Unavailable[HarnessCodex], "login") {
		t.Fatalf("a logged-out codex is advertised: %+v %v", caps, err)
	}
	if _, err := f.s.Start(ctx, f.r); err == nil || !strings.Contains(err.Error(), "does not offer harness codex") {
		t.Fatalf("start on a logged-out codex: %v", err)
	}
	t.Setenv("BEEKEEPER_FAKE_VERSION", "0.167.0")
	if caps, _ := f.s.Check(ctx); caps.Harnesses[HarnessCodex] != "" {
		t.Fatal("a codex whose version differs from the pinned one is advertised")
	}
}

func startCodex(t *testing.T, f fixture) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestCodexRunnerSingleTurn(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("execution runner uses a Linux user service")
	}
	f, dir := codexFixture(t)
	t.Setenv("OPENAI_API_KEY", "fixture-value")
	t.Setenv("CLAUDE_CONFIG_DIR", "/fixture")
	ctx := startCodex(t, f)
	if err := f.s.Run(ctx, f.r.Ref); err != nil {
		t.Fatal(err)
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil || !j.Ready || j.Phase != Exited || j.Thread != fixtureThread {
		t.Fatalf("single codex turn: %+v %v", j, err)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls")) //nolint:gosec // test-owned fake harness log
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(calls))
	want := f.s.codexHomeDir() + "|exec --json --strict-config --dangerously-bypass-approvals-and-sandbox --dangerously-bypass-hook-trust -m gpt-fixture -C " + j.Handle.Workspace + " -|" + f.r.Prompt
	if line != want {
		t.Fatalf("first turn:\n got %q\nwant %q", line, want)
	}
	if err := f.s.Run(ctx, f.r.Ref); err == nil {
		t.Fatal("runner launched twice")
	}
}

func TestCodexRunnerFailsWithoutThread(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("execution runner uses a Linux user service")
	}
	f, dir := codexFixture(t)
	ctx := startCodex(t, f)
	if err := os.WriteFile(filepath.Join(dir, "no-thread"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Run(ctx, f.r.Ref); err == nil {
		t.Fatal("a codex turn without a thread was accepted")
	}
	if j, _ := f.s.Read(f.r.Ref); j.Ready || j.Phase != Failed {
		t.Fatalf("failed first turn recorded as %s ready=%v", j.Phase, j.Ready)
	}
}

func TestCodexRunnerWakesResumeTheThread(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("execution runner uses a Linux user service")
	}
	f, dir := codexFixture(t)
	f.r.Messaging = true
	ctx := startCodex(t, f)
	done := make(chan error, 1)
	go func() { done <- f.s.Run(ctx, f.r.Ref) }()
	one := Envelope{ID: newID(), Sequence: 1, From: f.r.Ref, To: f.r.Ref, Kind: MessageWake, Text: "first follow-up", Sender: Sender{Name: "supervisor"}}
	two := one
	two.ID, two.Sequence, two.Text, two.Sender = newID(), 2, "second follow-up", Sender{}
	// The gap first, before the runner may even have bound its socket.
	if _, err := f.s.Message(ctx, f.r.Ref, two); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Message(ctx, f.r.Ref, one); err != nil {
		t.Fatal(err)
	}
	for {
		j, err := f.s.Read(f.r.Ref)
		if err != nil {
			t.Fatal(err)
		}
		if j.Messages.Sequence == 2 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("runner ended before its wakes: %v", err)
		case <-ctx.Done():
			t.Fatal("wakes were not accepted")
		case <-time.After(20 * time.Millisecond):
		}
	}
	for _, m := range []Envelope{one, two} {
		if ack, err := f.s.Message(ctx, f.r.Ref, m); err != nil || ack.Stage != Accepted {
			t.Fatalf("accepted wake resent: %+v %v", ack, err)
		}
	}
	// A finished report completes the execution; the idle runner exits.
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error { j.Completed = true; return nil }); err != nil {
		t.Fatal(err)
	}
	// finishControl wakes the runner the same way after the report.
	if conn, err := (&net.Dialer{}).DialContext(ctx, "unix", f.s.messageSocket(f.r.Ref)); err == nil {
		_ = conn.Close()
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("completed codex runner did not exit")
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls")) //nolint:gosec // test-owned fake harness log
	if err != nil {
		t.Fatal(err)
	}
	// Each call begins with the managed home; prompts may span lines.
	turns := strings.Split(strings.TrimSpace(string(calls)), f.s.codexHomeDir()+"|")[1:]
	for i := range turns {
		turns[i] = strings.TrimSpace(turns[i])
	}
	if len(turns) != 3 {
		t.Fatalf("want 3 turns, got %d: %q", len(turns), turns)
	}
	resume := "exec resume --json --strict-config --dangerously-bypass-approvals-and-sandbox --dangerously-bypass-hook-trust -m gpt-fixture " + fixtureThread + " -|"
	if !strings.HasPrefix(turns[1], resume+"Message from ") || !strings.Contains(turns[1], `"name":"supervisor"`) || !strings.HasSuffix(turns[1], "first follow-up") {
		t.Fatalf("first wake turn: %q", turns[1])
	}
	if !strings.HasSuffix(turns[2], resume+"second follow-up") {
		t.Fatalf("second wake turn out of order: %q", turns[2])
	}
	j, _ := f.s.Read(f.r.Ref)
	if j.Phase != Exited || j.Messages.Entries[one.ID].Stage != Accepted {
		t.Fatalf("final journal %s %+v", j.Phase, j.Messages.Entries[one.ID])
	}
}

func TestCodexRunnerRefusesForeignThread(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("execution runner uses a Linux user service")
	}
	f, dir := codexFixture(t)
	f.r.Messaging = true
	ctx := startCodex(t, f)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.s.Run(ctx, f.r.Ref) }()
	for {
		if j, _ := f.s.Read(f.r.Ref); j.Ready {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(dir, "foreign"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := Envelope{ID: newID(), Sequence: 1, From: f.r.Ref, To: f.r.Ref, Kind: MessageWake, Text: "follow-up"}
	if _, err := f.s.Message(ctx, f.r.Ref, m); err != nil {
		t.Fatal(err)
	}
	for {
		j, _ := f.s.Read(f.r.Ref)
		if j.Messages.Entries[m.ID].Stage == Uncertain && strings.Contains(j.LastError, "uncertain") {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("a turn on a foreign thread was not left uncertain")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if j, _ := f.s.Read(f.r.Ref); j.Messages.Sequence != 0 || j.Thread != fixtureThread {
		t.Fatal("a foreign thread was accepted or replaced the recorded one")
	}
	cancel()
	<-done
}

// A turn that fails after its acceptance ends a messaging runner as failed:
// acceptance is delivery evidence, not a finished turn, and a silent runner
// would keep its lease and the host's room with nobody to wake it.
func TestCodexRunnerSurfacesFailedTurns(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("execution runner uses a Linux user service")
	}
	for _, test := range []struct{ name, marker string }{{"first turn exits nonzero", "fail-turn"}, {"first turn reports turn.failed", "turn-failed"}} {
		t.Run(test.name, func(t *testing.T) {
			f, dir := codexFixture(t)
			f.r.Messaging = true
			ctx := startCodex(t, f)
			if err := os.WriteFile(filepath.Join(dir, test.marker), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := f.s.Run(ctx, f.r.Ref); err == nil {
				t.Fatal("a failed first turn left the runner waiting")
			}
			// The unit ends with its runner; the coordinator then observes the failure.
			f.launcher.mu.Lock()
			f.launcher.states[UnitName(f.r.Ref)] = unitInactive
			f.launcher.mu.Unlock()
			o, err := f.s.Inspect(ctx, f.r.Ref)
			if err != nil {
				t.Fatal(err)
			}
			if j, _ := f.s.Read(f.r.Ref); j.Phase != Failed || !j.Ready || j.LastError != codexTurnFailure || o.ProcessState != Failed {
				t.Fatalf("failed turn recorded as %s ready=%v %q, observed %s", j.Phase, j.Ready, j.LastError, o.ProcessState)
			}
		})
	}
	t.Run("wake turn exits nonzero", func(t *testing.T) {
		f, dir := codexFixture(t)
		f.r.Messaging = true
		ctx := startCodex(t, f)
		done := make(chan error, 1)
		go func() { done <- f.s.Run(ctx, f.r.Ref) }()
		for {
			if j, _ := f.s.Read(f.r.Ref); j.Ready && j.Thread != "" {
				break
			}
			select {
			case err := <-done:
				t.Fatalf("runner ended before its wake: %v", err)
			case <-ctx.Done():
				t.Fatal("first turn not accepted")
			case <-time.After(10 * time.Millisecond):
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "fail-turn"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		m := Envelope{ID: newID(), Sequence: 1, From: f.r.Ref, To: f.r.Ref, Kind: MessageWake, Text: "follow-up"}
		if _, err := f.s.Message(ctx, f.r.Ref, m); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a failed wake turn ended the runner as a success")
			}
		case <-ctx.Done():
			t.Fatal("a failed wake turn left the runner waiting")
		}
		j, _ := f.s.Read(f.r.Ref)
		if j.Phase != Failed || j.Messages.Entries[m.ID].Stage != Accepted {
			t.Fatalf("wake turn failure recorded as %s, receipt %s", j.Phase, j.Messages.Entries[m.ID].Stage)
		}
	})
}
