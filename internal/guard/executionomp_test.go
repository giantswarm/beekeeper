package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const (
	ompDeniedMerge = "gh pr merge 7 --repo giantswarm/beekeeper"
	ompDeniedPush  = "git push origin main"
)

func TestExecutionOmpGuard(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	h := Hook{Upstream: Upstream{Orgs: []string{allowedOrg}}}
	for _, tc := range []struct {
		name, tool string
		input      map[string]any
		denied     bool
	}{
		{"omp status", bashCmd, map[string]any{commandKey: codexGitStatus}, false},
		{"omp merge", bashCmd, map[string]any{commandKey: ompDeniedMerge}, true},
		{"default branch", bashCmd, map[string]any{commandKey: ompDeniedPush}, true},
		{"cwd outside", bashCmd, map[string]any{commandKey: codexGitStatus, ompWorkingDir: outside}, true},
		{"cwd symlink", bashCmd, map[string]any{commandKey: codexGitStatus, ompWorkingDir: "escape"}, true},
		{"background", bashCmd, map[string]any{commandKey: codexGitStatus, "async": true}, true},
		{"service", bashCmd, map[string]any{commandKey: codexGitStatus, nameKey: executionFixture}, true},
		{ompReadTool, ompReadTool, map[string]any{pathKey: "file.go"}, false},
		{ompWriteTool, ompWriteTool, map[string]any{pathKey: "file.go", contentKey: executionFixture}, false},
		{"read outside", ompReadTool, map[string]any{pathKey: filepath.Join(outside, "file")}, true},
		{"write symlink", ompWriteTool, map[string]any{pathKey: "escape/file", contentKey: executionFixture}, true},
		{"network read", ompReadTool, map[string]any{pathKey: "https://example.com"}, true},
		{"internal read", ompReadTool, map[string]any{pathKey: "agent://other"}, true},
		{"subagent", "task", map[string]any{}, true},
		{"python", "python", map[string]any{"code": "pass"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(event{ToolName: tc.tool, ToolInput: tc.input, CWD: workspace, Session: executionFixture})
			answer := h.DecideExecutionOmp(raw, executionFixture, workspace, nil)
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
		if json.Unmarshal(h.DecideExecutionOmp(raw, executionFixture, workspace, nil), &out) != nil || out.Specific.PermissionDecision != decisionDeny {
			t.Fatal("accepted invalid identity/input")
		}
	}
}
