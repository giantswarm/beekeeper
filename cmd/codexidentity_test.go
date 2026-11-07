package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/lease"
)

func TestCodexCallerAndLegacyLabClaim(t *testing.T) {
	const id, thread = "worker-start", "worker-thread"
	a := &app{cfg: &config.Config{StateDir: t.TempDir(), LeaseDir: t.TempDir(), Labs: map[string]string{"lab": "test-lab"}}}
	inbox := codex.InboxPath(a.cfg.StateDir, id)
	if err := os.MkdirAll(filepath.Dir(inbox), 0700); err != nil {
		t.Fatal(err)
	}
	if err := codex.RecordThread(a.cfg.StateDir, inbox, thread); err != nil {
		t.Fatal(err)
	}
	t.Setenv(codex.EnvAgent, id)
	t.Setenv(codex.EnvName, "worker")
	p, err := a.callerSession()
	if err != nil || p.Session != thread || p.HostSession != codex.HostPrefix+id {
		t.Fatalf("caller = %+v, %v", p, err)
	}
	// Existing claims have only the start ID, never a conversation ID.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agentlab.yaml"), []byte("clusterName: test-lab\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Dir(a.cfg.LeaseDir).Claim("lab", lease.Holder{HostSession: p.HostSession, Name: p.Name, LabConfig: &lease.LabConfig{Path: filepath.Join(dir, "agentlab.yaml")}}); err != nil {
		t.Fatal(err)
	}
	h := guard.Hook{Labs: a.heldLabs}
	for _, session := range []string{thread, "another-thread", ""} {
		raw, err := json.Marshal(map[string]any{
			"session_id": session, "hook_event_name": "PreToolUse", "cwd": dir,
			"tool_name": "Bash", "tool_input": map[string]any{"command": "agentlab configure --lab test-lab"},
		})
		if err != nil {
			t.Fatal(err)
		}
		out := string(h.Decide(raw))
		if denied := strings.Contains(out, `"permissionDecision":"deny"`); denied != (session != thread) {
			t.Fatalf("session %q: %s", session, out)
		}
	}
	// Nested commands can recover the registered host identity from their
	// thread even when the worker-specific environment is absent.
	t.Setenv(codex.EnvAgent, "")
	t.Setenv(codex.EnvName, "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", thread)
	nested, err := a.callerSession()
	if err != nil || nested.Session != thread || nested.HostSession != p.HostSession {
		t.Fatalf("nested caller = %+v, %v", nested, err)
	}
	t.Setenv("CODEX_THREAD_ID", "unregistered-thread")
	if _, err := a.callerSession(); err == nil {
		t.Fatal("unregistered thread accepted as a caller")
	}
}
