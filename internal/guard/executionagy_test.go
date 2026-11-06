package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func executionAgyEvent(tool string, args map[string]any, session string, workspaces ...string) []byte {
	m := map[string]any{
		transcriptConversationKey: session,
		toolCallField: map[string]any{
			nameKey:   tool,
			argsField: args,
		},
	}
	if len(workspaces) > 0 {
		m["workspacePaths"] = workspaces
	}
	b, _ := json.Marshal(m)
	return b
}

func executionAgyDenied(t *testing.T, b []byte) bool {
	t.Helper()
	var out agyDecision
	if json.Unmarshal(b, &out) != nil || out.Decision == "" {
		t.Fatalf("agy guard answer is not agy hook JSON: %s", b)
	}
	return out.Decision == decisionDeny
}

func TestExecutionAgyGuard(t *testing.T) {
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
	const subDir = "sub"
	sub := filepath.Join(workspace, subDir)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(sub, "sub-escape")); err != nil {
		t.Fatal(err)
	}
	realHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(protected, "execution", "agy-home"))
	t.Setenv(executionHomeEnv, realHome)
	if ExecutionRulesAllowed(filepath.Join(realHome, ".ssh", "id_ed25519"), nil) {
		t.Fatal("ExecutionRulesAllowed accepted host user's .ssh key under agy HOME override")
	}
	h := Hook{Upstream: Upstream{Orgs: []string{allowedOrg}}}
	decide := func(raw []byte) bool {
		return executionAgyDenied(t, h.DecideExecutionAgy(raw, executionThread, workspace, []string{protected}))
	}
	const missingThreadCase = "missing agy thread"
	denied := map[string][]byte{
		"invalid JSON":             []byte("{"),
		"another session":          executionAgyEvent("run_command", map[string]any{cmdLineKey: codexGitStatus}, "019a-other", workspace),
		missingThreadCase:          nil,
		"workspacePath outside":    executionAgyEvent("run_command", map[string]any{cmdLineKey: codexGitStatus}, executionThread, outside),
		"workspacePath tilde":      executionAgyEvent("run_command", map[string]any{cmdLineKey: codexGitStatus}, executionThread, "~/outside"),
		cwdOutsideCaseName:         executionAgyEvent("run_command", map[string]any{cmdLineKey: codexGitStatus, agyCwdKey: outside}, executionThread, workspace),
		"cwd tilde":                executionAgyEvent("run_command", map[string]any{cmdLineKey: codexGitStatus, agyCwdKey: "~/outside"}, executionThread, workspace),
		"bash directoryPath out":   executionAgyEvent("run_command", map[string]any{cmdLineKey: codexGitStatus, agyDirPathKey: outside}, executionThread, workspace),
		"bash searchPath out":      executionAgyEvent("run_command", map[string]any{cmdLineKey: codexGitStatus, agyCwdKey: workspace, agySearchPathKey: outside}, executionThread, workspace),
		"daemon command":           executionAgyEvent("run_command", map[string]any{cmdLineKey: codexGitStatus, "IsDaemon": true}, executionThread, workspace),
		"pr merge":                 executionAgyEvent("run_command", map[string]any{cmdLineKey: "gh pr merge 8 --repo giantswarm/beekeeper"}, executionThread, workspace),
		"push to main":             executionAgyEvent("run_command", map[string]any{cmdLineKey: "git push origin HEAD:refs/heads/main"}, executionThread, workspace),
		"harness lifecycle":        executionAgyEvent("run_command", map[string]any{cmdLineKey: "agy --version"}, executionThread, workspace),
		"identity override":        executionAgyEvent("run_command", map[string]any{cmdLineKey: "env BEEKEEPER_CONFIG=/srv/other.yaml beekeeper agents idle"}, executionThread, workspace),
		"home identity override":   executionAgyEvent("run_command", map[string]any{cmdLineKey: "env BEEKEEPER_EXECUTION_HOME=/srv/other git status"}, executionThread, workspace),
		"protected write":          executionAgyEvent("run_command", map[string]any{cmdLineKey: "rm " + filepath.Join(protected, "policy.json")}, executionThread, workspace),
		"host secret via HOME":     executionAgyEvent("run_command", map[string]any{cmdLineKey: "cat $HOME/.config/gh/hosts.yml"}, executionThread, workspace),
		"write outside":            executionAgyEvent("write_file", map[string]any{filePathKey: filepath.Join(outside, "file")}, executionThread, workspace),
		"write tilde":              executionAgyEvent("write_file", map[string]any{filePathKey: "~/outside-file"}, executionThread, workspace),
		"write through symlink":    executionAgyEvent("write_file", map[string]any{filePathKey: "escape/new-file"}, executionThread, workspace),
		"write sub symlink":        executionAgyEvent("write_file", map[string]any{agyDirPathKey: subDir, filePathKey: "sub-escape/new-file"}, executionThread, workspace),
		"edit outside":             executionAgyEvent("edit_file", map[string]any{filePathKey: filepath.Join(outside, "file"), "Target": "a", "ReplacementContent": "b"}, executionThread, workspace),
		"read outside":             executionAgyEvent("read_file", map[string]any{filePathKey: filepath.Join(outside, "file")}, executionThread, workspace),
		"read tilde":               executionAgyEvent("read_file", map[string]any{filePathKey: "~/.ssh/id_ed25519"}, executionThread, workspace),
		"read sub symlink":         executionAgyEvent("read_file", map[string]any{agyDirPathKey: subDir, filePathKey: "sub-escape/file"}, executionThread, workspace),
		"grep outside":             executionAgyEvent("grep_search", map[string]any{agySearchPathKey: outside, patternKey: "x"}, executionThread, workspace),
		"grep tilde":               executionAgyEvent("grep_search", map[string]any{agySearchPathKey: "~", patternKey: "x"}, executionThread, workspace),
		"find outside":             executionAgyEvent("find_by_name", map[string]any{agySearchDirKey: outside, patternKey: "*"}, executionThread, workspace),
		"find tilde":               executionAgyEvent("find_by_name", map[string]any{agySearchDirKey: "~/outside", patternKey: "*"}, executionThread, workspace),
		"typed task input":         executionAgyEvent(manageTaskTool, map[string]any{actionField: sendInputAction, taskIDField: "t1", inputField: "gh pr merge 9\n"}, executionThread, workspace),
		"send_command_input":       executionAgyEvent("send_command_input", map[string]any{"CommandId": "t1", inputField: "gh pr merge 10\n"}, executionThread, workspace),
		"ask_question unsupported": executionAgyEvent("ask_question", map[string]any{}, executionThread, workspace),
		"send_message unsupported": executionAgyEvent("send_message", map[string]any{"Recipient": "supervisor", messageField: "hi"}, executionThread, workspace),
		"subagent unsupported":     executionAgyEvent("invoke_subagent", map[string]any{"TypeName": "research"}, executionThread, workspace),
		"unauthenticated mcp":      executionAgyEvent("call_mcp_tool", map[string]any{"server": "coordinator", "tool": "ping"}, executionThread, workspace),
		"unknown tool":             executionAgyEvent("future_tool", map[string]any{"q": executionFixture}, executionThread, workspace),
		"forbidden upstream issue": executionAgyEvent("run_command", map[string]any{cmdLineKey: "gh issue create --repo forbidden/other --title fixture --body fixture"}, executionThread, workspace),
	}
	for name, raw := range denied {
		session := executionThread
		if name == missingThreadCase {
			raw, session = executionAgyEvent("run_command", map[string]any{cmdLineKey: codexGitStatus}, "", workspace), ""
		}
		if !executionAgyDenied(t, h.DecideExecutionAgy(raw, session, workspace, []string{protected})) {
			t.Errorf("%s was not denied", name)
		}
	}
	allowed := map[string][]byte{
		"workspace status":  executionAgyEvent("run_command", map[string]any{cmdLineKey: codexGitStatus}, executionThread, workspace),
		"home expansion":    executionAgyEvent("run_command", map[string]any{cmdLineKey: "echo $HOME"}, executionThread, workspace),
		"component park":    executionAgyEvent("run_command", map[string]any{cmdLineKey: "beekeeper agents park --on giantswarm/beekeeper#1 'desk review'"}, executionThread, workspace),
		"workspace write":   executionAgyEvent("write_file", map[string]any{filePathKey: "new-file.txt", contentKey: "ok"}, executionThread, workspace),
		"workspace edit":    executionAgyEvent("edit_file", map[string]any{filePathKey: "new-file.txt", "Target": "a", "new_string": "b"}, executionThread, workspace),
		"workspace read":    executionAgyEvent("read_file", map[string]any{filePathKey: filepath.Join(workspace, "new-file.txt")}, executionThread, workspace),
		"workspace grep":    executionAgyEvent("grep_search", map[string]any{agySearchPathKey: workspace, patternKey: "fixture"}, executionThread, workspace),
		"workspace find":    executionAgyEvent("find_by_name", map[string]any{agySearchDirKey: ".", patternKey: "*.go"}, executionThread, workspace),
		"manage_task list":  executionAgyEvent(manageTaskTool, map[string]any{actionField: "list"}, executionThread, workspace),
		"terminate_command": executionAgyEvent("terminate_command", map[string]any{"CommandId": "t1"}, executionThread, workspace),
		"web search":        executionAgyEvent("search_web", map[string]any{"q": executionFixture}, executionThread, workspace),
	}
	for name, raw := range allowed {
		if decide(raw) {
			t.Errorf("%s was denied: %s", name, h.DecideExecutionAgy(raw, executionThread, workspace, []string{protected}))
		}
	}
	mcpHook := h
	mcpHook.ExecutionMCP = true
	if executionAgyDenied(t, mcpHook.DecideExecutionAgy(executionAgyEvent("call_mcp_tool", map[string]any{"server": "coordinator", "tool": "ping"}, executionThread, workspace), executionThread, workspace, []string{protected})) {
		t.Fatal("coordinator MCP tool call was denied when ExecutionMCP is enabled")
	}
	if !executionAgyDenied(t, ExecutionDenyAgy("fixture")) {
		t.Fatal("agy denial is not an agy deny decision")
	}
}
