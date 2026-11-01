package guard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	transcriptOutputCount     = "count"
	transcriptOwner           = "granted-session"
	transcriptOther           = "foreign-session"
	transcriptWorker          = "reader-session"
	transcriptReasonName      = "private harness transcripts"
	transcriptConversationKey = "conversationId"
	transcriptAbsolutePath    = "AbsolutePath"
)

func transcriptHook() Hook {
	h := hook()
	h.Transcripts.Managed = func(string) (bool, error) { return true, nil }
	return h
}

func TestTranscriptReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "worker-codex"))
	h := transcriptHook()
	paths := []string{
		"~/.codex/sessions/2026/rollout-other.jsonl", "~/.codex/archived_sessions/other.jsonl", "~/.codex/history.jsonl",
		"$CODEX_HOME/sessions/other.jsonl", "${CODEX_HOME}/archived_sessions/other.jsonl",
		"~/.claude/projects/project/other.jsonl", "~/.claude/projects/project/other/tool-results/result.txt",
		"~/.gemini/antigravity-cli/brain/other/.system_generated/logs/transcript.jsonl",
		"~/.gemini/antigravity-cli/conversations/other.db", "~/.gemini/tmp/project/chats/session.json",
		"~/.omp/agent/sessions/project/other.jsonl",
	}
	for _, p := range paths {
		for _, command := range []string{
			"cat " + p, "jq 'keys' " + p, "grep -c token_count " + p, "cp " + p + " /tmp/copy",
			"cat " + p + " | wc -l", "cat " + p + " >/dev/null", "bash -c 'head " + p + "'",
			"python3 -c 'print(open(\"" + p + "\").read())'", "f=" + p + "; cat $f",
		} {
			d := decide(t, h, home, command, nil)
			if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, transcriptReasonName) {
				t.Errorf("not refused: %s: %+v", command, d)
			}
		}
	}
	for _, command := range []string{"rg token_count ~/.codex", "cat ~/.codex/sessions/**/*.jsonl", "cd ~/.codex/sessions && head other.jsonl", "find ~/.claude/projects -exec cat {} +"} {
		d := decide(t, h, home, command, nil)
		if d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("not refused: %s", command)
		}
	}
}

func transcriptDecision(t *testing.T, h Hook, session, tool, p string) *hookOutput {
	t.Helper()
	b, err := json.Marshal(map[string]any{sessionKey: session, toolNameKey: tool, toolInputKey: map[string]any{filePathKey: p, pathKey: p, outputModeKey: transcriptOutputCount}})
	if err != nil {
		t.Fatal(err)
	}
	out := h.Decide(b)
	if out == nil {
		return nil
	}
	var response struct {
		Output hookOutput `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatal(err)
	}
	return &response.Output
}

func TestTranscriptFileToolsAndGrants(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".codex", "sessions")
	own := filepath.Join(root, "own.jsonl")
	other := filepath.Join(root, "other.jsonl")
	fixture := filepath.Join(root, "fixture")
	h := transcriptHook()
	h.Transcripts.Grants = map[string][]string{transcriptOwner: {own, fixture}}
	for _, tc := range []struct {
		session, tool, p string
		deny             bool
	}{
		{transcriptOwner, readTool, own, false}, {transcriptOther, readTool, own, true}, {"", readTool, own, true},
		{transcriptOwner, readTool, other, true}, {transcriptOwner, readTool, filepath.Join(fixture, "shape.json"), false},
		{transcriptOwner, grepTool, root, true}, {transcriptOwner, globTool, root, true},
		{transcriptOwner, readTool, filepath.Join(home, "fixture.json"), false},
	} {
		d := transcriptDecision(t, h, tc.session, tc.tool, tc.p)
		if denied := d != nil && d.PermissionDecision == decisionDeny; denied != tc.deny {
			t.Errorf("%+v: %+v", tc, d)
		}
	}
	h.ConfigErr = errors.New("invalid policy")
	if d := transcriptDecision(t, h, transcriptOwner, readTool, own); d == nil || d.PermissionDecision != decisionDeny {
		t.Fatal("configuration failure passed")
	}
}

func TestTranscriptLinks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".claude", "projects")
	fixture := filepath.Join(root, "fixture")
	if err := os.MkdirAll(fixture, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(home, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, transcriptOther), filepath.Join(fixture, "escape")); err != nil {
		t.Fatal(err)
	}
	h := transcriptHook()
	h.Transcripts.Grants = map[string][]string{transcriptOwner: {fixture}}
	for _, p := range []string{filepath.Join(home, "alias", "missing.jsonl"), filepath.Join(fixture, "escape", "missing.jsonl")} {
		if d := transcriptDecision(t, h, transcriptOwner, readTool, p); d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("link passed: %s", p)
		}
	}
}

func TestTranscriptHarnessFormats(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, ".codex", "sessions", "other.jsonl")
	h := transcriptHook()
	cases := []struct {
		format  string
		payload map[string]any
	}{
		{secretLinkCodex, map[string]any{hookEventKey: PreToolUseEvent, sessionKey: transcriptWorker, toolNameKey: "exec_command", toolInputKey: map[string]any{"cmd": "jq 'keys' " + p}}},
		{secretLinkCodex, map[string]any{hookEventKey: PreToolUseEvent, sessionKey: transcriptWorker, toolNameKey: "write_stdin", toolInputKey: map[string]any{"chars": "cat " + p + "\n"}}},
		{secretLinkCodex, map[string]any{hookEventKey: PreToolUseEvent, sessionKey: transcriptWorker, toolNameKey: "view_image", toolInputKey: map[string]any{pathKey: p}}},
		{"agy", map[string]any{transcriptConversationKey: transcriptWorker, "toolCall": map[string]any{"name": "run_command", "args": map[string]any{"CommandLine": "cat " + p}}}},
		{"agy", map[string]any{transcriptConversationKey: transcriptWorker, "toolCall": map[string]any{"name": "view_file", "args": map[string]any{transcriptAbsolutePath: p}}}},
	}
	for _, tc := range cases {
		raw, err := json.Marshal(tc.payload)
		if err != nil {
			t.Fatal(err)
		}
		var out []byte
		if tc.format == secretLinkCodex {
			out = h.DecideCodex(raw)
		} else {
			out = h.DecideAgy(raw)
		}
		if !strings.Contains(string(out), transcriptReasonName) || !strings.Contains(string(out), "deny") {
			t.Errorf("%s: %s", raw, out)
		}
	}
}

func TestTranscriptShellGrants(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, ".omp", "agent", "sessions", "own.jsonl")
	h := transcriptHook()
	h.Transcripts.Grants = map[string][]string{transcriptOwner: {p}}
	for _, session := range []string{transcriptOwner, transcriptOther, ""} {
		ev := toolEvent(bashTool, map[string]any{commandKey: "cat " + p})
		ev[sessionKey] = session
		d := decideEvent(t, h, ev)
		if denied := d != nil && d.PermissionDecision == decisionDeny; denied != (session != transcriptOwner) {
			t.Errorf("%q: %+v", session, d)
		}
	}
	// An event cannot confer access just by claiming a transcript_path.
	ev := toolEvent(readTool, map[string]any{filePathKey: p})
	ev[sessionKey] = transcriptOther
	ev["transcript_path"] = p
	if d := decideEvent(t, h, ev); d == nil || d.PermissionDecision != decisionDeny {
		t.Fatal("self-reported transcript granted access")
	}
}

func TestTranscriptAmbiguousReaders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h := transcriptHook()
	for _, command := range []string{
		`python3 -c 'from pathlib import Path; print((Path.home()/".codex"/"sessions").rglob("*"))'`,
		`echo(){ cat "$1"; }; echo ~/.codex/sessions/other.jsonl`,
		`ls(){ cat "$1"; }; ls ~/.codex/sessions/other.jsonl`,
		`reader ~/.codex`,
		`cat <<EOF
$(cat ~/.codex/history.jsonl)
EOF`,
	} {
		d := decide(t, h, home, command, nil)
		if d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("reader passed: %s", command)
		}
	}
	t.Setenv("HOME", "")
	if d := transcriptDecision(t, h, transcriptOwner, readTool, "/unknown"); d == nil || d.PermissionDecision != decisionDeny {
		t.Fatal("missing home passed")
	}
}

func TestTranscriptOwnArtifacts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h := transcriptHook()
	customClaude := filepath.Join(home, "custom-projects")
	customBrain := filepath.Join(home, "custom-brain")
	h.Transcripts.ClaudeProjects = []string{customClaude}
	h.Transcripts.AgyBrains = []string{customBrain}
	for _, root := range []string{filepath.Join(home, ".claude", "projects"), customClaude} {
		own := filepath.Join(root, "project", transcriptOwner, "tool-results", "result.txt")
		other := filepath.Join(root, "project", transcriptOther, "tool-results", "result.txt")
		for _, tool := range []string{readTool, editTool, multiEditTool, writeTool, grepTool, globTool} {
			if d := transcriptDecision(t, h, transcriptOwner, tool, own); d != nil && d.PermissionDecision == decisionDeny {
				t.Errorf("own spill refused by %s: %+v", tool, d)
			}
			if d := transcriptDecision(t, h, transcriptOwner, tool, other); d == nil || d.PermissionDecision != decisionDeny {
				t.Errorf("foreign spill passed by %s", tool)
			}
		}
		ev := toolEvent(bashTool, map[string]any{commandKey: "cat " + own})
		ev[sessionKey] = transcriptOwner
		if d := decideEvent(t, h, ev); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("own spill shell read refused: %+v", d)
		}
		// Ownership cannot be conferred by a symlink in the session directory.
		if err := os.MkdirAll(filepath.Dir(own), 0o700); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(filepath.Dir(own), "foreign.txt")
		if err := os.Symlink(other, alias); err != nil {
			t.Fatal(err)
		}
		if d := transcriptDecision(t, h, transcriptOwner, readTool, alias); d == nil || d.PermissionDecision != decisionDeny {
			t.Fatal("own spill symlink escaped")
		}
		for _, id := range []string{"", "../" + transcriptOwner, transcriptOther} {
			if d := transcriptDecision(t, h, id, readTool, own); d == nil || d.PermissionDecision != decisionDeny {
				t.Errorf("invalid/foreign session %q owns spill", id)
			}
		}
	}
	for _, root := range []string{filepath.Join(home, ".gemini", "antigravity-cli", "brain"), customBrain} {
		own := filepath.Join(root, transcriptOwner, "task.md")
		other := filepath.Join(root, transcriptOther, "task.md")
		for _, tool := range []string{readTool, writeTool, editTool, multiEditTool} {
			if d := transcriptDecision(t, h, transcriptOwner, tool, own); d != nil && d.PermissionDecision == decisionDeny {
				t.Errorf("own agy artifact refused by %s", tool)
			}
			if d := transcriptDecision(t, h, transcriptOwner, tool, other); d == nil || d.PermissionDecision != decisionDeny {
				t.Errorf("foreign agy artifact passed by %s", tool)
			}
		}
		patch := "*** Begin Patch\n*** Update File: " + own + "\n@@\n-old\n+new\n*** End Patch\n"
		if reason, _ := h.checkCodexApplyPatch(event{Session: transcriptOwner}, patch, home); reason != "" {
			t.Errorf("own artifact patch refused: %s", reason)
		}
	}
}

func TestTranscriptManagedScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, ".codex", "sessions", "foreign.jsonl")
	for _, managed := range []bool{false, true} {
		h := hook()
		h.Transcripts.Managed = func(string) (bool, error) { return managed, nil }
		if d := transcriptDecision(t, h, transcriptOwner, readTool, p); (d != nil && d.PermissionDecision == decisionDeny) != managed {
			t.Errorf("file managed=%v: %+v", managed, d)
		}
		ev := toolEvent(bashTool, map[string]any{commandKey: "cat " + p})
		ev[sessionKey] = transcriptOwner
		if d := decideEvent(t, h, ev); (d != nil && d.PermissionDecision == decisionDeny) != managed {
			t.Errorf("shell managed=%v: %+v", managed, d)
		}
	}
	h := hook()
	h.Transcripts.Managed = func(string) (bool, error) { return false, errors.New("unreadable starts") }
	if d := transcriptDecision(t, h, transcriptOwner, readTool, p); d == nil || d.PermissionDecision != decisionDeny {
		t.Fatal("unreadable starts passed")
	}
}
