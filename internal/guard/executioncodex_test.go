package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const (
	executionThread  = "019a0000-0000-7000-8000-000000000001"
	codexExecCommand = "exec_command"
	codexGitStatus   = "git status"
)

func executionCodexEvent(tool string, input map[string]any, session, cwd string) []byte {
	b, _ := json.Marshal(map[string]any{hookEventKey: PreToolUseEvent, toolNameKey: tool, toolInputKey: input, sessionKey: session, executionWorkingDir: cwd})
	return b
}

func executionCodexDenied(t *testing.T, b []byte) bool {
	t.Helper()
	var out codexOutput
	if json.Unmarshal(b, &out) != nil {
		t.Fatalf("codex guard answer is not codex hook JSON: %s", b)
	}
	return out.Specific.PermissionDecision == decisionDeny
}

func TestExecutionCodexGuard(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	protected, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Skip("symlink unavailable")
	}
	h := Hook{Upstream: Upstream{Orgs: []string{allowedOrg}}}
	decide := func(raw []byte) bool {
		return executionCodexDenied(t, h.DecideExecutionCodex(raw, executionThread, workspace, []string{protected}))
	}
	patch := func(target string) string {
		return "*** Begin Patch\n*** Add File: " + target + "\n+fixture\n*** End Patch\n"
	}
	denied := map[string][]byte{
		"invalid JSON":             []byte("{"),
		"another session":          executionCodexEvent(codexExecCommand, map[string]any{codexCmdKey: codexGitStatus}, "019a-other", workspace),
		"no recorded thread":       nil,
		"cwd outside":              executionCodexEvent(codexExecCommand, map[string]any{codexCmdKey: codexGitStatus}, executionThread, outside),
		"workdir outside":          executionCodexEvent(codexExecCommand, map[string]any{codexCmdKey: codexGitStatus, "workdir": outside}, executionThread, workspace),
		"merge":                    executionCodexEvent(codexExecCommand, map[string]any{codexCmdKey: "gh pr merge 7 --repo giantswarm/beekeeper"}, executionThread, workspace),
		"push to main":             executionCodexEvent("shell", map[string]any{commandKey: "git push origin HEAD:refs/heads/main"}, executionThread, workspace),
		"harness lifecycle":        executionCodexEvent(codexExecCommand, map[string]any{codexCmdKey: "codex exec resume --last"}, executionThread, workspace),
		"identity override":        executionCodexEvent(codexExecCommand, map[string]any{codexCmdKey: "BEEKEEPER_EXECUTION_ID=other beekeeper agents idle"}, executionThread, workspace),
		"protected write":          executionCodexEvent(codexExecCommand, map[string]any{codexCmdKey: "rm " + filepath.Join(protected, "policy.json")}, executionThread, workspace),
		"patch outside":            executionCodexEvent(codexApplyPatchTool, map[string]any{codexInputKey: patch(filepath.Join(outside, "file"))}, executionThread, workspace),
		"patch through symlink":    executionCodexEvent(codexApplyPatchTool, map[string]any{codexInputKey: patch("escape/new-file")}, executionThread, workspace),
		"typed stdin":              executionCodexEvent(codexWriteStdinTool, map[string]any{codexCharsKey: "gh pr merge 7\n", sessionKey: 1}, executionThread, workspace),
		"image outside":            executionCodexEvent(codexViewImageTool, map[string]any{pathKey: filepath.Join(outside, "a.png")}, executionThread, workspace),
		"unsupported tool":         executionCodexEvent("web_search", map[string]any{"q": executionFixture}, executionThread, workspace),
		"claude-format file tool":  executionCodexEvent("mcp__server__tool", map[string]any{}, executionThread, workspace),
		"forbidden upstream issue": executionCodexEvent(codexExecCommand, map[string]any{codexCmdKey: "gh issue create --repo forbidden/repository --title fixture --body fixture"}, executionThread, workspace),
	}
	for name, raw := range denied {
		session := executionThread
		if name == "no recorded thread" {
			raw, session = executionCodexEvent(codexExecCommand, map[string]any{codexCmdKey: codexGitStatus}, "", workspace), ""
		}
		if !executionCodexDenied(t, h.DecideExecutionCodex(raw, session, workspace, []string{protected})) {
			t.Errorf("%s was not denied", name)
		}
	}
	allowed := map[string][]byte{
		"status":             executionCodexEvent(codexExecCommand, map[string]any{codexCmdKey: codexGitStatus, "workdir": workspace}, executionThread, workspace),
		"component park":     executionCodexEvent(codexExecCommand, map[string]any{codexCmdKey: "beekeeper agents park --on giantswarm/beekeeper#1 'desk review'"}, executionThread, workspace),
		"workspace patch":    executionCodexEvent(codexApplyPatchTool, map[string]any{codexInputKey: patch("new-file.txt")}, executionThread, workspace),
		"poll a command":     executionCodexEvent(codexWriteStdinTool, map[string]any{codexCharsKey: "", sessionKey: 1}, executionThread, workspace),
		"update plan":        executionCodexEvent(codexUpdatePlanTool, map[string]any{"steps": []any{}}, executionThread, workspace),
		"image in workspace": executionCodexEvent(codexViewImageTool, map[string]any{pathKey: "a.png"}, executionThread, workspace),
	}
	for name, raw := range allowed {
		if decide(raw) {
			t.Errorf("%s was denied: %s", name, h.DecideExecutionCodex(raw, executionThread, workspace, []string{protected}))
		}
	}
	if !executionCodexDenied(t, ExecutionDenyCodex("fixture")) {
		t.Fatal("codex denial is not a codex deny decision")
	}
}
