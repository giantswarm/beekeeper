package execution

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/agyhooks"
)

const fixtureAgyVersion = "1.4.7"

const agyScript = `#!/bin/sh
set -eu
calls="$(dirname "$0")/calls"
if [ "$1" = --version ]; then echo "agy version ${BEEKEEPER_FAKE_VERSION:-1.4.7}"; exit 0; fi
if [ "$1" = models ]; then
  if [ -e "$(dirname "$0")/logged-out" ]; then echo "Not logged in" >&2; exit 1; fi
  echo "gemini-3.1-pro-high"; exit 0
fi
test -n "${HOME}"
test -n "${BEEKEEPER_EXECUTION_HOME}"
test "${HOME}" != "${BEEKEEPER_EXECUTION_HOME}"
test -z "${GEMINI_API_KEY+x}"
test -z "${GOOGLE_API_KEY+x}"
test -z "${CODEX_HOME+x}"
test -z "${CLAUDE_CONFIG_DIR+x}"
test -n "${BEEKEEPER_EXECUTION_ID}"
test -n "${BEEKEEPER_CONFIG}"
IFS= read -r input
case "$input" in
  '{"event":"user","message":{"role":"user","content":'*) ;;
  *) echo '{"event":"result","result":{"status":"ERROR","error":"stream input message is missing the \"event\" field"}}'; exit 1 ;;
esac
printf '%s|%s|%s\n' "$HOME" "$*" "$input" >> "$calls"
if [ -e "$(dirname "$0")/no-thread" ]; then echo '{"event":"error"}'; exit 1; fi
thread="019a0000-0000-7000-8000-000000000001"
prev=""
for a in "$@"; do
  if [ "$prev" = --conversation ]; then thread="$a"; fi
  prev="$a"
done
if [ -e "$(dirname "$0")/foreign" ]; then thread="019a0000-0000-7000-8000-00000000ffff"; fi
printf '{"event":"init","conversation_id":"%s"}\n' "$thread"
printf '{"event":"step_update","step_update":{"conversation_id":"%s","step_type":"user_input"}}\n' "$thread"
if [ -e "$(dirname "$0")/fail-turn" ]; then exit 1; fi
if [ -e "$(dirname "$0")/turn-failed" ]; then printf '{"event":"result","result":{"conversation_id":"%s","status":"error"}}\n' "$thread"; exit 0; fi
printf '{"event":"result","result":{"conversation_id":"%s","status":"done"}}\n' "$thread"
`

func agyFixture(t *testing.T) (fixture, string) {
	t.Helper()
	f := testFixture(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "agy")
	if err := os.WriteFile(bin, []byte(agyScript), 0o700); err != nil { //nolint:gosec // executable fake harness in the test's own temporary directory
		t.Fatal(err)
	}
	f.s.Config.Harnesses[HarnessAgy] = HarnessConfig{Executable: bin, Version: fixtureAgyVersion, ConfigDir: filepath.Join(dir, "login")}
	digest, err := f.s.Prepare(f.s.Policy())
	if err != nil {
		t.Fatal(err)
	}
	f.r.Harness, f.r.Model, f.r.PolicyDigest = HarnessAgy, "gemini-3.1-pro-high", digest
	return f, dir
}

func TestAgyHostConfigHarnesses(t *testing.T) {
	f := testFixture(t)
	agy := HarnessConfig{Executable: "/usr/local/bin/agy", Version: fixtureAgyVersion, ConfigDir: "/srv/agy"}
	for name, test := range map[string]struct {
		harnesses map[string]HarnessConfig
		ok        bool
	}{
		"agy only":            {map[string]HarnessConfig{HarnessAgy: agy}, true},
		"claude and agy":      {map[string]HarnessConfig{HarnessClaude: f.s.Config.Harnesses[HarnessClaude], HarnessAgy: agy}, true},
		"agy without login":   {map[string]HarnessConfig{HarnessAgy: {Executable: agy.Executable, Version: agy.Version}}, false},
		"agy below its range": {map[string]HarnessConfig{HarnessAgy: {Executable: agy.Executable, Version: "1.4.0", ConfigDir: agy.ConfigDir}}, false},
		"agy relative binary": {map[string]HarnessConfig{HarnessAgy: {Executable: "agy", Version: agy.Version, ConfigDir: agy.ConfigDir}}, false},
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
	}{{[]string{HarnessAgy}, true}, {[]string{HarnessClaude, HarnessAgy}, true}, {[]string{HarnessClaude, HarnessCodex, HarnessAgy}, true}, {[]string{HarnessAgy, HarnessAgy}, false}} {
		h := Host{Endpoint: Endpoint{SSH: "host-agy.example", Executable: "/usr/local/bin/beekeeper", Config: "/etc/beekeeper/worker.json"}, Identity: fixtureHost, WorkspaceRoot: "/srv/workspaces", Repositories: map[string]string{f.r.Repository: "/srv/source"}, Harnesses: test.harnesses, PolicyDigest: Digest([]byte("policy")), MaxBusy: 1}
		if err := h.Validate(); (err == nil) != test.ok {
			t.Fatalf("host harnesses %v: %v, want ok %v", test.harnesses, err, test.ok)
		}
	}
}

func TestPrepareInstallsManagedAgyHome(t *testing.T) {
	plain := testFixture(t)
	if plain.s.Policy().AgyDigest != "" {
		t.Fatal("a claude-only host policy binds an agy home")
	}
	f, dir := agyFixture(t)
	p, err := f.s.InstalledPolicy()
	if err != nil || p.AgyDigest == "" {
		t.Fatalf("agy policy not installed: %v", err)
	}
	cfgDir := f.s.agyConfigDir()
	hooks, err := os.ReadFile(filepath.Join(cfgDir, hooksJSONFile)) //nolint:gosec // test-owned managed home
	if err != nil || !strings.Contains(string(hooks), "agents execution-guard --format agy") || !strings.Contains(string(hooks), "PreToolUse") {
		t.Fatalf("agy hooks do not run the execution guard: %s %v", hooks, err)
	}
	var parsed map[string]map[string][]map[string]any
	if err := json.Unmarshal(hooks, &parsed); err != nil || len(parsed["beekeeper"]["PreToolUse"]) != 1 {
		t.Fatalf("agy hooks do not match the beekeeper PreToolUse shape: %s %v", hooks, err)
	}
	if rules, err := os.ReadFile(filepath.Join(cfgDir, "GEMINI.md")); err != nil || string(rules) != string(f.s.Rules) { //nolint:gosec // test-owned managed home
		t.Fatal("agy home does not carry the embedded worker rules")
	}
	if target, err := os.Readlink(filepath.Join(f.s.agyAppDir(), agyLoginToken)); err != nil || target != filepath.Join(dir, "login", agyLoginToken) {
		t.Fatalf("agy login is not linked from the host's own configDir: %q %v", target, err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, hooksJSONFile), []byte(`{"beekeeper":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.InstalledPolicy(); err == nil {
		t.Fatal("tampered agy hooks still match the policy")
	}
}

// The managed hooks.json is in the shape agy accepts, and the agy probe of
// `agents hosts check` and of every start refuses agy while agy would reject
// that file: agy drops the whole file for one bad entry and runs unguarded.
func TestAgyProbeRefusesHooksAgyRejects(t *testing.T) {
	f, _ := agyFixture(t)
	ctx := context.Background()
	if why := agyhooks.Rejects(f.s.agyFiles()[hooksJSONFile]); why != "" {
		t.Fatalf("agy rejects the managed hooks.json: %s", why)
	}
	h := f.s.Config.Harnesses[HarnessAgy]
	if err := f.s.probe(ctx, HarnessAgy, h, &HarnessReadiness{}); err != nil {
		t.Fatalf("probe of the prepared home: %v", err)
	}
	path := filepath.Join(f.s.agyConfigDir(), hooksJSONFile)
	stale := `{"beekeeper":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"/opt/bin/beekeeper agents execution-guard --format agy"}]}]},` +
		`"stale":{"PreInvocation":[{"matcher":"*","hooks":[{"type":"command","command":"/usr/local/bin/stale"}]}]}}`
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	readiness := HarnessReadiness{Guard: probePassed}
	err := f.s.probe(ctx, HarnessAgy, h, &readiness)
	if readiness.Guard != probeFailed {
		t.Fatalf("rejected hooks reported a passing guard: %+v", readiness)
	}
	if err == nil || !strings.Contains(err.Error(), "agy would reject the managed hooks.json and run unguarded") || !strings.Contains(err.Error(), `the "stale" entry`) {
		t.Fatalf("probe of a rejected hooks.json: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := f.s.probe(ctx, HarnessAgy, h, &HarnessReadiness{}); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("probe without hooks.json: %v", err)
	}
}

func TestCheckAdvertisesPassingAgyOnly(t *testing.T) {
	f, dir := agyFixture(t)
	ctx := context.Background()
	caps, err := f.s.Check(ctx)
	if err != nil || caps.Harnesses[HarnessAgy] != fixtureAgyVersion || caps.Harnesses[HarnessClaude] == "" || caps.Unavailable != nil {
		t.Fatalf("both harnesses should pass: %+v %v", caps, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logged-out"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	caps, err = f.s.Check(ctx)
	if err != nil || caps.Harnesses[HarnessAgy] != "" || caps.Harnesses[HarnessClaude] == "" || !strings.Contains(caps.Unavailable[HarnessAgy], "login") {
		t.Fatalf("a logged-out agy is advertised: %+v %v", caps, err)
	}
	if _, err := f.s.Start(ctx, f.r); err == nil || !strings.Contains(err.Error(), "does not offer harness agy") {
		t.Fatalf("start on a logged-out agy: %v", err)
	}
	t.Setenv("BEEKEEPER_FAKE_VERSION", "1.4.8")
	if caps, _ := f.s.Check(ctx); caps.Harnesses[HarnessAgy] != "" {
		t.Fatal("an agy whose version differs from the pinned one is advertised")
	}
}

func TestAgyRunnerSingleTurn(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("execution runner uses a Linux user service")
	}
	f, dir := agyFixture(t)
	t.Setenv("GEMINI_API_KEY", "fixture-value")
	t.Setenv("GOOGLE_API_KEY", "fixture-value")
	t.Setenv("CODEX_HOME", "/fixture")
	t.Setenv("CLAUDE_CONFIG_DIR", "/fixture")
	ctx := startCodex(t, f)
	if err := f.s.Run(ctx, f.r.Ref); err != nil {
		t.Fatal(err)
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil || !j.Ready || j.Phase != Exited || j.Thread != fixtureThread {
		t.Fatalf("single agy turn: %+v %v", j, err)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls")) //nolint:gosec // test-owned fake harness log
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(calls))
	msg := strings.TrimSpace(string(agyStreamMessage(f.r.Prompt)))
	want := f.s.agyHomeDir() + "|--input-format stream-json --output-format stream-json --model gemini-3.1-pro-high --dangerously-skip-permissions --new-project|" + msg
	if line != want {
		t.Fatalf("first turn:\n got %q\nwant %q", line, want)
	}
	if err := f.s.Run(ctx, f.r.Ref); err == nil {
		t.Fatal("runner launched twice")
	}
}

func TestAgyRunnerFailsWithoutThread(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("execution runner uses a Linux user service")
	}
	f, dir := agyFixture(t)
	ctx := startCodex(t, f)
	if err := os.WriteFile(filepath.Join(dir, "no-thread"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Run(ctx, f.r.Ref); err == nil {
		t.Fatal("an agy turn without a conversation id was accepted")
	}
	if j, _ := f.s.Read(f.r.Ref); j.Ready || j.Phase != Failed {
		t.Fatalf("failed first turn recorded as %s ready=%v", j.Phase, j.Ready)
	}
}

func TestAgyRunnerWakesResumeTheConversation(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("execution runner uses a Linux user service")
	}
	f, dir := agyFixture(t)
	f.r.Messaging = true
	ctx := startCodex(t, f)
	done := make(chan error, 1)
	go func() { done <- f.s.Run(ctx, f.r.Ref) }()
	one := Envelope{ID: newID(), Sequence: 1, From: f.r.Ref, To: f.r.Ref, Kind: MessageWake, Text: "first agy follow-up", Sender: Sender{Name: "lead"}}
	two := one
	two.ID, two.Sequence, two.Text, two.Sender = newID(), 2, "second agy follow-up", Sender{}
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
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error { j.Completed = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if conn, err := (&net.Dialer{}).DialContext(ctx, "unix", f.s.messageSocket(f.r.Ref)); err == nil {
		_ = conn.Close()
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("completed agy runner did not exit")
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls")) //nolint:gosec // test-owned fake harness log
	if err != nil {
		t.Fatal(err)
	}
	turns := strings.Split(strings.TrimSpace(string(calls)), f.s.agyHomeDir()+"|")[1:]
	for i := range turns {
		turns[i] = strings.TrimSpace(turns[i])
	}
	if len(turns) != 3 {
		t.Fatalf("want 3 turns, got %d: %q", len(turns), turns)
	}
	resume := "--input-format stream-json --output-format stream-json --model gemini-3.1-pro-high --dangerously-skip-permissions --new-project --conversation " + fixtureThread + "|"
	if !strings.HasPrefix(turns[1], resume) || !strings.Contains(turns[1], "Message from ") || !strings.Contains(turns[1], "lead") || !strings.Contains(turns[1], "first agy follow-up") {
		t.Fatalf("first wake turn: %q", turns[1])
	}
	if !strings.HasPrefix(turns[2], resume) || !strings.Contains(turns[2], "second agy follow-up") {
		t.Fatalf("second wake turn out of order: %q", turns[2])
	}
}

func TestAgyRunnerRefusesForeignConversation(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("execution runner uses a Linux user service")
	}
	f, dir := agyFixture(t)
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
	m := Envelope{ID: newID(), Sequence: 1, From: f.r.Ref, To: f.r.Ref, Kind: MessageWake, Text: "agy follow-up"}
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
			t.Fatal("a turn on a foreign conversation was not left uncertain")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if j, _ := f.s.Read(f.r.Ref); j.Messages.Sequence != 0 || j.Thread != fixtureThread {
		t.Fatal("a foreign conversation was accepted or replaced the recorded one")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled runner: %v", err)
	}
}

func TestAgyRunnerSurfacesFailedTurns(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("execution runner uses a Linux user service")
	}
	for _, test := range []struct{ name, marker string }{{"first turn exits nonzero", "fail-turn"}, {"first turn reports error status", "turn-failed"}} {
		t.Run(test.name, func(t *testing.T) {
			f, dir := agyFixture(t)
			f.r.Messaging = true
			ctx := startCodex(t, f)
			if err := os.WriteFile(filepath.Join(dir, test.marker), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := f.s.Run(ctx, f.r.Ref); err == nil {
				t.Fatal("a failed first turn left the runner waiting")
			}
			f.launcher.mu.Lock()
			f.launcher.states[UnitName(f.r.Ref)] = unitInactive
			f.launcher.mu.Unlock()
			o, err := f.s.Inspect(ctx, f.r.Ref)
			if err != nil {
				t.Fatal(err)
			}
			if j, _ := f.s.Read(f.r.Ref); j.Phase != Failed || !j.Ready || j.LastError != agyTurnFailure || o.ProcessState != Failed {
				t.Fatalf("failed turn recorded as %s ready=%v %q, observed %s", j.Phase, j.Ready, j.LastError, o.ProcessState)
			}
		})
	}
}

func TestAgyRunnerCoordinatorMCP(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("execution runner uses a Linux user service")
	}
	f, _ := agyFixture(t)
	f.r.MCP = true
	ctx := startCodex(t, f)
	if err := f.s.Run(ctx, f.r.Ref); err != nil {
		t.Fatal(err)
	}
	mcpFile := filepath.Join(f.s.Root, string(f.r.Ref.Execution)+"-agy-home", ".gemini", "config", "mcp_config.json")
	raw, err := os.ReadFile(mcpFile) //nolint:gosec // test-owned execution directory
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{`"coordinator"`, `"mcp-proxy"`, EnvID, EnvCoordinator, f.s.Executable, f.s.ConfigPath} {
		if !strings.Contains(string(raw), part) {
			t.Fatalf("proxy setting %q missing in %s", part, raw)
		}
	}
	sharedMCP, err := os.ReadFile(filepath.Join(f.s.agyConfigDir(), "mcp_config.json")) //nolint:gosec // test-owned managed home
	if err != nil || strings.TrimSpace(string(sharedMCP)) != `{"mcpServers":{}}` {
		t.Fatalf("shared agy policy home was mutated: %s %v", sharedMCP, err)
	}
	f.launcher.mu.Lock()
	f.launcher.states[UnitName(f.r.Ref)] = unitInactive
	f.launcher.mu.Unlock()
	if _, err := f.s.Stop(ctx, f.r.Ref, StopRequest{ID: "stop-mcp", Reason: "done"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(filepath.Dir(mcpFile)))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("per-execution MCP home was not cleaned on stop: %v", err)
	}
}
