package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const (
	executionToolInput = "tool_input"
	executionToolName  = "tool_name"
	executionFixture   = "fixture"
)

func executionEvent(tool string, input map[string]any, cwd string) []byte {
	b, _ := json.Marshal(map[string]any{executionToolName: tool, executionToolInput: input, sessionKey: executionFixture, "cwd": cwd})
	return b
}

func executionDenied(b []byte) bool {
	var out struct {
		Hook hookOutput `json:"hookSpecificOutput"`
	}
	return json.Unmarshal(b, &out) == nil && out.Hook.PermissionDecision == decisionDeny
}

func TestExecutionGuardFailsClosedAndRefusesRemoteAuthority(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	protected, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := Hook{Upstream: Upstream{Orgs: []string{allowedOrg}}}
	for _, cmd := range []string{
		"gh pr merge 7 --repo giantswarm/beekeeper",
		"env gh pr merge 7 --repo giantswarm/beekeeper",
		"env -- gh pr merge 7 --repo giantswarm/beekeeper",
		"timeout 5s command gh pr merge 7 --repo giantswarm/beekeeper",
		"BEEKEEPER_EXECUTION_ID=other beekeeper agents idle",
		"env -u BEEKEEPER_CONFIG beekeeper agents idle",
		"env -i beekeeper agents idle",
		"unset BEEKEEPER_EXECUTION_ID",
		"bash -c 'gh pr merge 7 --repo giantswarm/beekeeper'",
		"gh api -X PUT repos/giantswarm/beekeeper/pulls/7/merge",
		"gh api graphql -f query='mutation { mergePullRequest(input: {}) { clientMutationId } }'",
		"gh api graphql -F query=@request.json",
		"git push origin HEAD:main",
		"gh issue create --repo forbidden/repository --title fixture --body fixture",
		"systemctl --user stop personal.service",
		"claude stop personal-conversation",
		"beekeeper --as supervisor note add fixture",
		"printf fixture 2>" + filepath.Join(protected, "policy.json"),
		"rm " + filepath.Join(protected, "policy.json"),
	} {
		if !executionDenied(h.DecideExecution(executionEvent(bashTool, map[string]any{commandKey: cmd}, workspace), executionFixture, workspace, []string{protected})) {
			t.Errorf("command was not denied: %s", cmd)
		}
	}
	for _, raw := range [][]byte{[]byte("{"), executionEvent("UnknownTool", map[string]any{}, workspace), executionEvent(bashTool, map[string]any{commandKey: flagOn}, protected)} {
		if !executionDenied(h.DecideExecution(raw, executionFixture, workspace, nil)) {
			t.Error("invalid or unsupported event was not denied")
		}
	}
	if executionDenied(h.DecideExecution(executionEvent(bashTool, map[string]any{commandKey: "gh pr create --repo giantswarm/beekeeper --title fixture --body 'Documents gh pr merge behavior'"}, workspace), executionFixture, workspace, []string{protected})) {
		t.Fatal("merge words in prose were treated as a command")
	}
}

func TestExecutionFileToolsStayInTheirWorkspace(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
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
	h := Hook{}
	for _, p := range []string{"", filepath.Join(outside, "file"), filepath.Join(workspace, "escape", "new-file")} {
		if !executionDenied(h.DecideExecution(executionEvent(writeTool, map[string]any{"file_path": p, contentKey: executionFixture}, workspace), executionFixture, workspace, nil)) {
			t.Error("file tool escaped workspace")
		}
	}
	if executionDenied(h.DecideExecution(executionEvent(writeTool, map[string]any{"file_path": filepath.Join(workspace, "new-file"), contentKey: executionFixture}, workspace), executionFixture, workspace, nil)) {
		t.Fatal("ordinary workspace write was denied")
	}
}

func TestExecutionBashProtectsRelativeAndSymlinkTargets(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	protected, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(protected, filepath.Join(workspace, "escape")); err != nil {
		t.Skip("symlink unavailable")
	}
	h := Hook{}
	relative, err := filepath.Rel(workspace, filepath.Join(protected, "policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"cat " + relative, "printf fixture 3>escape/policy.json", "rm escape/policy.json"} {
		if !executionDenied(h.DecideExecution(executionEvent(bashTool, map[string]any{commandKey: command}, workspace), executionFixture, workspace, []string{protected})) {
			t.Errorf("protected target admitted: %s", command)
		}
	}
}

func TestExecutionRulesRefuseKnownCredentialStoresBeforeRead(t *testing.T) {
	root := t.TempDir()
	credential := filepath.Join(root, "credential-store.json")
	ordinary := filepath.Join(root, "worker-rules.md")
	if err := os.WriteFile(credential, []byte(executionFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(credential, ordinary); err != nil {
		t.Skip("symlink unavailable")
	}
	if ExecutionRulesAllowed(ordinary, nil) {
		t.Fatal("rules symlink named a credential store")
	}
	if ExecutionRulesAllowed(filepath.Join(root, "auth-data.json"), []string{filepath.Join(root, "*.json")}) {
		t.Fatal("configured credential glob was ignored")
	}
}
