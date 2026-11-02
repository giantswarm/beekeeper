package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestExecutionOmpGuard(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	h := Hook{Upstream: Upstream{Orgs: []string{"giantswarm"}}}
	for _, tc := range []struct {
		name, tool string
		input      map[string]any
		denied     bool
	}{
		{"status", "bash", map[string]any{"command": "git status"}, false},
		{"merge", "bash", map[string]any{"command": "gh pr merge 7 --repo giantswarm/beekeeper"}, true},
		{"default branch", "bash", map[string]any{"command": "git push origin main"}, true},
		{"cwd outside", "bash", map[string]any{"command": "git status", "cwd": outside}, true},
		{"cwd symlink", "bash", map[string]any{"command": "git status", "cwd": "escape"}, true},
		{"background", "bash", map[string]any{"command": "git status", "async": true}, true},
		{"service", "bash", map[string]any{"command": "git status", "name": "fixture"}, true},
		{"read", "read", map[string]any{"path": "file.go"}, false},
		{"write", "write", map[string]any{"path": "file.go", "content": "fixture"}, false},
		{"read outside", "read", map[string]any{"path": filepath.Join(outside, "file")}, true},
		{"write symlink", "write", map[string]any{"path": "escape/file", "content": "fixture"}, true},
		{"network read", "read", map[string]any{"path": "https://example.com"}, true},
		{"internal read", "read", map[string]any{"path": "agent://other"}, true},
		{"subagent", "task", map[string]any{}, true},
		{"python", "python", map[string]any{"code": "pass"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(event{ToolName: tc.tool, ToolInput: tc.input, CWD: workspace, Session: "fixture"})
			answer := h.DecideExecutionOmp(raw, "fixture", workspace, nil)
			var out struct {
				Specific hookOutput `json:"hookSpecificOutput"`
			}
			if len(answer) > 0 && json.Unmarshal(answer, &out) != nil {
				t.Fatalf("invalid answer %q", answer)
			}
			if got := out.Specific.PermissionDecision == decisionDeny; got != tc.denied {
				t.Fatalf("denied=%v want %v: %s", got, tc.denied, answer)
			}
		})
	}
	for _, raw := range [][]byte{[]byte("{"), []byte(`{"session_id":"other","tool_name":"bash","tool_input":{"command":"git status"}}`)} {
		var out struct {
			Specific hookOutput `json:"hookSpecificOutput"`
		}
		if json.Unmarshal(h.DecideExecutionOmp(raw, "fixture", workspace, nil), &out) != nil || out.Specific.PermissionDecision != decisionDeny {
			t.Fatal("accepted invalid identity/input")
		}
	}
}
