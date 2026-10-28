package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
)

func TestMergeConfigTOML(t *testing.T) {
	// User config has top-level keys and [features]/[mcp_servers] both before
	// and after [projects.*] sections.
	userTOML := `
model = "gpt-5-interactive"
approval_policy = "on-request"
project_doc_max_bytes = 4096

[features]
hooks = false
plugins = true
apps = true
multi_agent = true

[mcp_servers.first]
command = "npx"
args = ["-y", "first-mcp"]

[projects."/work/repo-alpha"]
trust_level = "trusted"

[features.extra]
ignored = true

[projects."/work/repo-beta"]
trust_level = "untrusted"

[mcp_servers.second]
command = "npx"
args = ["-y", "second-mcp"]
`
	merged, err := MergeConfigTOML([]byte(userTOML))
	if err != nil {
		t.Fatalf("MergeConfigTOML failed: %v", err)
	}

	var parsed map[string]any
	if err := toml.Unmarshal(merged, &parsed); err != nil {
		t.Fatalf("unmarshaling merged TOML: %v\n%s", err, string(merged))
	}

	if got := parsed["project_doc_max_bytes"]; got != int64(DefaultProjectDocMaxBytes) {
		t.Errorf("project_doc_max_bytes = %v, want %d", got, DefaultProjectDocMaxBytes)
	}
	if _, exists := parsed["model"]; exists {
		t.Errorf("unexpected top-level model in worker config: %v", parsed["model"])
	}
	if _, exists := parsed["mcp_servers"]; exists {
		t.Errorf("unexpected mcp_servers in worker config: %v", parsed["mcp_servers"])
	}

	sep, _ := parsed["shell_environment_policy"].(map[string]any)
	if sep["inherit"] != "all" {
		t.Errorf("shell_environment_policy.inherit = %v, want all", sep["inherit"])
	}

	feat, _ := parsed["features"].(map[string]any)
	wantFeatures := map[string]bool{
		"hooks":             true,
		"daemon_auto_start": false,
		"plugins":           false,
		"apps":              false,
		"multi_agent":       false,
		"multi_agent_v2":    false,
		"unified_exec_tty":  false,
	}
	for k, want := range wantFeatures {
		if got, ok := feat[k].(bool); !ok || got != want {
			t.Errorf("features.%s = %v, want %v", k, feat[k], want)
		}
	}

	projects, _ := parsed["projects"].(map[string]any)
	alpha, _ := projects["/work/repo-alpha"].(map[string]any)
	beta, _ := projects["/work/repo-beta"].(map[string]any)
	if alpha["trust_level"] != "trusted" {
		t.Errorf("projects./work/repo-alpha.trust_level = %v, want trusted", alpha["trust_level"])
	}
	if beta["trust_level"] != "untrusted" {
		t.Errorf("projects./work/repo-beta.trust_level = %v, want untrusted", beta["trust_level"])
	}

	if _, err := MergeConfigTOML([]byte("not = [valid toml")); err == nil {
		t.Errorf("expected error on invalid TOML, got nil")
	}
}

func TestEnsureHome(t *testing.T) {
	root := t.TempDir()
	userCodex := filepath.Join(root, "user-codex")
	if err := os.MkdirAll(userCodex, 0o700); err != nil {
		t.Fatal(err)
	}
	authFile := filepath.Join(userCodex, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"token":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	userConfig := filepath.Join(userCodex, "config.toml")
	if err := os.WriteFile(userConfig, []byte("[projects.\"/tmp/work\"]\ntrust_level = \"trusted\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	codexHome := HomeDir(filepath.Join(root, "state"))
	if err := EnsureHome(codexHome, authFile, userConfig, "/usr/local/bin/beekeeper", "unalias grep\n"); err != nil {
		t.Fatalf("EnsureHome: %v", err)
	}

	linkTarget, err := os.Readlink(filepath.Join(codexHome, "auth.json"))
	if err != nil || linkTarget != authFile {
		t.Errorf("auth.json symlink = %q (err %v), want %q", linkTarget, err, authFile)
	}
	hooksRaw, err := os.ReadFile(filepath.Join(codexHome, "hooks.json")) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hooksRaw), "/usr/local/bin/beekeeper hook pretooluse --format codex") {
		t.Errorf("unexpected hooks.json: %s", string(hooksRaw))
	}
	zshenvRaw, err := os.ReadFile(filepath.Join(ZshDir(codexHome), ".zshenv")) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	zshenv := string(zshenvRaw)
	if !strings.Contains(zshenv, "ZDOTDIR=\"$HOME\"\n[[ -f \"$HOME/.zshenv\" ]] && source \"$HOME/.zshenv\"\nunalias grep") {
		t.Errorf("unexpected .zshenv ordering: %s", zshenv)
	}
	zprofileRaw, err := os.ReadFile(filepath.Join(ZshDir(codexHome), ".zprofile")) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	zprofile := string(zprofileRaw)
	if !strings.Contains(zprofile, "ZDOTDIR=\"$HOME\"\n[[ -f \"$HOME/.zprofile\" ]] && source \"$HOME/.zprofile\"\nunalias grep") {
		t.Errorf("unexpected .zprofile ordering: %s", zprofile)
	}
}

const (
	testModel     = "gpt-5.7"
	sampleRollout = `{"timestamp":"2026-10-28T10:00:00Z","type":"session_meta","payload":{"id":"019a296d-aa93-71d2-842b-dc8079ce03a4","timestamp":"2026-10-28T10:00:00Z","cwd":"/work/repo","git":{"branch":"feat/codex"}}}
{"timestamp":"2026-10-28T10:00:01Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions>bypass</permissions instructions>"}]}}
{"timestamp":"2026-10-28T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /work/repo\n\n<INSTRUCTIONS>\nrules\n</INSTRUCTIONS>"}]}}
{"timestamp":"2026-10-28T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Worker rules header\n\nTask:\nImplement the codex harness in https://github.com/giantswarm/beekeeper/issues/1708\nDetails here"}]}}
{"timestamp":"2026-10-28T10:00:02Z","type":"turn_context","payload":{"cwd":"/work/repo","model":"gpt-5.7"}}
{"timestamp":"2026-10-28T10:00:05Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"gh issue view 1708 --repo giantswarm/beekeeper\"}","call_id":"call_1"}}
{"timestamp":"2026-10-28T10:00:10Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"Process exited with code 1\nOutput:\nFAIL\n"}}
{"timestamp":"2026-10-28T10:00:12Z","type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","call_id":"call_2","input":"*** Begin Patch\n*** Update File: internal/codex/codex.go\n@@\n*** End Patch"}}
{"timestamp":"2026-10-28T10:00:13Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_2","output":"Success. Updated the following files:\nM internal/codex/codex.go\n"}}
{"timestamp":"2026-10-28T10:00:15Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":200,"total_tokens":1200},"last_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":200,"total_tokens":1200}}}}
{"timestamp":"2026-10-28T10:00:15Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":200,"total_tokens":1200},"last_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":200,"total_tokens":1200}}}}
{"timestamp":"2026-10-28T10:00:16Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"All tests pass now."}]}}
{"timestamp":"2026-10-28T10:00:16Z","type":"event_msg","payload":{"type":"task_complete"}}
`
)

func TestReadTranscriptTailFollowSpend(t *testing.T) {
	stateDir := t.TempDir()
	sessionsDir := HomeSessionsDir(stateDir)
	dayDir := filepath.Join(sessionsDir, "2026", "10", "28")
	if err := os.MkdirAll(dayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(dayDir, "rollout-2026-10-28T10-00-00-019a296d-aa93-71d2-842b-dc8079ce03a4.jsonl")
	if err := os.WriteFile(rolloutPath, []byte(sampleRollout), 0o600); err != nil {
		t.Fatal(err)
	}

	now, _ := time.Parse(time.RFC3339, "2026-10-28T10:05:00Z")
	work, act := ReadTranscript(rolloutPath, now)
	if act.Model != testModel {
		t.Errorf("act.Model = %q, want %s", act.Model, testModel)
	}
	if act.Total.Turns != 1 {
		t.Errorf("act.Total.Turns = %d, want 1", act.Total.Turns)
	}
	if act.Total.ToolCalls != 2 {
		t.Errorf("act.Total.ToolCalls = %d, want 2", act.Total.ToolCalls)
	}
	if act.Total.GitHubCalls != 1 {
		t.Errorf("act.Total.GitHubCalls = %d, want 1", act.Total.GitHubCalls)
	}
	if act.Total.ToolErrors != 1 {
		t.Errorf("act.Total.ToolErrors = %d, want 1", act.Total.ToolErrors)
	}
	wantTokens := claude.Tokens{Input: 400, Output: 200, CacheRead: 600}
	if act.Total.Tokens != wantTokens {
		t.Errorf("act.Total.Tokens = %+v, want %+v", act.Total.Tokens, wantTokens)
	}
	if act.Context != 1200 {
		t.Errorf("act.Context = %d, want 1200", act.Context)
	}
	if len(work.Refs) == 0 && len(work.Mentioned) == 0 {
		t.Errorf("expected work refs from rollout transcript, got %+v", work)
	}

	gotTail, err := Tail(rolloutPath, 10)
	if err != nil {
		t.Fatalf("Tail error: %v", err)
	}
	if len(gotTail) != 2 || gotTail[0].Role != roleUser || gotTail[1].Role != roleAssistant || gotTail[1].Text != "All tests pass now." {
		t.Errorf("Tail = %+v", gotTail)
	}

	gotFollow, err := Follow(rolloutPath, 10)
	if err != nil {
		t.Fatalf("Follow error: %v", err)
	}
	if len(gotFollow) != 4 || gotFollow[1].Role != claude.RoleTool || gotFollow[2].Role != claude.RoleTool {
		t.Errorf("Follow = %+v", gotFollow)
	}

	spends := SpendTranscripts(stateDir, "", time.Time{}, now.Add(time.Hour))
	if len(spends) != 1 || len(spends[0].Requests) != 1 {
		t.Fatalf("SpendTranscripts = %+v, want 1 transcript with 1 request", spends)
	}
	if spends[0].Requests[0].Model != testModel || spends[0].Requests[0].Tokens != wantTokens {
		t.Errorf("spend request = %+v", spends[0].Requests[0])
	}
}

func TestDiscoverAndEnded(t *testing.T) {
	stateDir := t.TempDir()
	dayDir := filepath.Join(HomeSessionsDir(stateDir), "2026", "10", "28")
	if err := os.MkdirAll(dayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(dayDir, "rollout-2026-10-28T10-00-00-019a296d-aa93-71d2-842b-dc8079ce03a4.jsonl")
	if err := os.WriteFile(rolloutPath, []byte(sampleRollout), 0o600); err != nil {
		t.Fatal(err)
	}

	agentID := "11111111-2222-3333-4444-555555555555"
	inbox := InboxPath(stateDir, agentID)
	if err := RecordThread(stateDir, inbox, "019a296d-aa93-71d2-842b-dc8079ce03a4"); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	pRunner := &proc.Process{
		PID:   100,
		PPID:  1,
		Comm:  "beekeeper",
		Args:  []string{"beekeeper", "codex-run", inbox, LogPath(stateDir, agentID), Comm, testModel},
		Start: now.Add(-time.Minute),
	}
	pCodex := &proc.Process{
		PID:   101,
		PPID:  100,
		Comm:  Comm,
		Args:  []string{Comm, "exec", "--json", "-m", testModel, "-"},
		Start: now.Add(-30 * time.Second),
	}
	pZsh := &proc.Process{
		PID:   102,
		PPID:  101,
		Comm:  "zsh",
		Args:  []string{"zsh", "-lc", "sleep 120"},
		Start: now.Add(-10 * time.Second),
	}
	pSleep := &proc.Process{
		PID:   103,
		PPID:  102,
		Comm:  "sleep",
		Args:  []string{"sleep", "120"},
		Start: now.Add(-10 * time.Second),
	}
	pAppServer := &proc.Process{
		PID:   200,
		PPID:  1,
		Comm:  Comm,
		Args:  []string{Comm, "app-server"},
		Start: now.Add(-time.Minute),
	}
	tbl := proc.NewTable(pRunner, pCodex, pZsh, pSleep, pAppServer)

	running := Discover(stateDir, "", tbl, now)
	if len(running) != 1 {
		t.Fatalf("Discover len = %d, want 1: %+v", len(running), running)
	}
	s := running[0]
	if s.Harness != Harness || s.State != StateBusy || s.Model != testModel {
		t.Errorf("unexpected session: %+v", s)
	}
	if len(s.Commands) != 1 || s.Commands[0].Args != "sleep 120" {
		t.Errorf("s.Commands = %+v, want [sleep 120]", s.Commands)
	}

	// When the child codex process exits between turns, codex-run remains idle
	// and live.
	tblIdle := proc.NewTable(pRunner, pAppServer)
	idleSessions := Discover(stateDir, "", tblIdle, now)
	if len(idleSessions) != 1 || idleSessions[0].State != StateIdle {
		t.Fatalf("idle Discover = %+v, want 1 idle session", idleSessions)
	}

	// Once codex-run exits too, Ended reports the session with its HostID.
	ended := Ended(stateDir, "", nil, now.Add(-24*time.Hour))
	if len(ended) != 1 || ended[0].HostID != HostPrefix+agentID || ended[0].State != StateEnded {
		t.Fatalf("Ended = %+v, want 1 ended session with HostID %s", ended, HostPrefix+agentID)
	}
}

func TestReadTranscriptPartialWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-partial.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	padding := `{"timestamp":"2026-10-28T09:00:00Z","type":"turn_context","payload":{"cwd":"/work/repo","model":"gpt-5.7","pad":"` + strings.Repeat("x", 4096) + `"}}` + "\n"
	for written := 0; written <= 512<<10+64*1024; written += len(padding) {
		if _, err := f.WriteString(padding); err != nil {
			t.Fatal(err)
		}
	}
	tailEvents := `{"timestamp":"2026-10-28T10:00:10Z","type":"turn_context","payload":{"cwd":"/work/repo","model":"gpt-5.7"}}
{"timestamp":"2026-10-28T10:00:15Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":50000,"cached_input_tokens":40000,"output_tokens":5000,"total_tokens":55000},"last_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":200,"total_tokens":1200}}}}
{"timestamp":"2026-10-28T10:00:20Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":51500,"cached_input_tokens":41000,"output_tokens":5300,"total_tokens":56800},"last_token_usage":{"input_tokens":1500,"cached_input_tokens":1000,"output_tokens":300,"total_tokens":1800}}}}
`
	if _, err := f.WriteString(tailEvents); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	now, _ := time.Parse(time.RFC3339, "2026-10-28T10:05:00Z")
	_, act := ReadTranscript(path, now)
	if act.Whole {
		t.Fatalf("expected partial window (!act.Whole)")
	}
	// First event in partial window uses last_token_usage (400 uncached + 600 cached, 200 out);
	// second event uses delta from seeded prevTotal (500 uncached + 1000 cached, 300 out).
	want := claude.Tokens{Input: 900, Output: 500, CacheRead: 1600}
	if act.Total.Tokens != want {
		t.Errorf("act.Total.Tokens = %+v, want %+v", act.Total.Tokens, want)
	}
}
