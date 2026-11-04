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
	for _, path := range []string{filepath.Join(workspace, "file.go"), filepath.Join(workspace, "www.example.com"), filepath.Join(outside, "file")} {
		if err := os.WriteFile(path, []byte("harmless path fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The spelling fallback must not follow an alternate symlink outside.
	if err := os.Symlink(filepath.Join(outside, "file"), filepath.Join(workspace, "cafe\u0301")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "src/file.go"), []byte("fixture"), 0o600); err != nil {
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
		{cwdOutsideCaseName, bashCmd, map[string]any{commandKey: codexGitStatus, ompWorkingDir: outside}, true},
		{"cwd symlink", bashCmd, map[string]any{commandKey: codexGitStatus, ompWorkingDir: "escape"}, true},
		{"background", bashCmd, map[string]any{commandKey: codexGitStatus, "async": true}, true},
		{"service", bashCmd, map[string]any{commandKey: codexGitStatus, nameKey: executionFixture}, true},
		{ompReadTool, ompReadTool, map[string]any{pathKey: "file.go"}, false},
		{ompWriteTool, ompWriteTool, map[string]any{pathKey: "file.go", contentKey: executionFixture}, false},
		{"read outside", ompReadTool, map[string]any{pathKey: filepath.Join(outside, "file")}, true},
		{"write symlink", ompWriteTool, map[string]any{pathKey: "escape/file", contentKey: executionFixture}, true},
		{"network read", ompReadTool, map[string]any{pathKey: "https://example.com"}, true},
		{"internal read", ompReadTool, map[string]any{pathKey: "agent://other"}, true},
		{"read at absolute path", ompReadTool, map[string]any{pathKey: "@" + filepath.Join(outside, "file")}, true},
		{"write at absolute path", ompWriteTool, map[string]any{pathKey: "@" + filepath.Join(outside, "file"), contentKey: executionFixture}, true},
		{"read home shorthand", ompReadTool, map[string]any{pathKey: "~/file"}, true},
		{"write home shorthand", ompWriteTool, map[string]any{pathKey: "@~/file", contentKey: executionFixture}, true},
		{"read path list", ompReadTool, map[string]any{pathKey: "file.go;" + filepath.Join(outside, "file")}, true},
		{"read comma list", ompReadTool, map[string]any{pathKey: "file.go," + filepath.Join(outside, "file")}, true},
		{"read space list", ompReadTool, map[string]any{pathKey: "file.go " + filepath.Join(outside, "file")}, true},
		{"read tab list", ompReadTool, map[string]any{pathKey: "file.go\t" + filepath.Join(outside, "file")}, true},
		{"read Unicode space", ompReadTool, map[string]any{pathKey: "file.go\u00a0" + filepath.Join(outside, "file")}, true},
		{"missing read", ompReadTool, map[string]any{pathKey: "missing.go"}, true},
		{"read Unicode fallback symlink", ompReadTool, map[string]any{pathKey: "caf\u00e9"}, true},
		{"read URL shorthand", ompReadTool, map[string]any{pathKey: "www.example.com"}, true},
		{"new literal write", ompWriteTool, map[string]any{pathKey: "new.go", contentKey: executionFixture}, false},
		{"read pattern", ompReadTool, map[string]any{pathKey: "*.go"}, true},
		{"write Unicode space", ompWriteTool, map[string]any{pathKey: "file\u202fname", contentKey: executionFixture}, true},
		{"literal nested read", ompReadTool, map[string]any{pathKey: "src/file.go"}, false},
		{"literal absolute write", ompWriteTool, map[string]any{pathKey: filepath.Join(workspace, "file.go"), contentKey: executionFixture}, false},
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
