package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const (
	executionToolInput          = "tool_input"
	executionToolName           = "tool_name"
	executionFixture            = "fixture"
	executionWorkingDir         = "cwd"
	literalViewerGraphQLCommand = `gh api graphql -f query='{ viewer { login } }'`
)

func executionEvent(tool string, input map[string]any, cwd string) []byte {
	b, _ := json.Marshal(map[string]any{executionToolName: tool, executionToolInput: input, sessionKey: executionFixture, executionWorkingDir: cwd})
	return b
}

func executionDenied(b []byte) bool {
	var out struct {
		Hook hookOutput `json:"hookSpecificOutput"`
	}
	return json.Unmarshal(b, &out) == nil && out.Hook.PermissionDecision == decisionDeny
}

// A read-only GraphQL query passes where every field of it is literal; the
// board hook's own hint recommends one. A write, or what the guard cannot read,
// stays with the coordinator.
func TestExecutionGuardAllowsLiteralGraphQLReads(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := Hook{Upstream: Upstream{Orgs: []string{allowedOrg}}}
	const query = "query='query($n:Int!){repository(owner:\"giantswarm\",name:\"beekeeper\"){issue(number:$n){projectItems(first:5){nodes{id}}}}}'"
	for _, cmd := range []string{
		"gh api graphql -f " + query + " -F n=3199",
		"gh api graphql --raw-field " + query + " --field n=3199 --jq .data",
		"gh api graphql -F " + query + " -F n=3199 --paginate --slurp",
		literalViewerGraphQLCommand,
		"GH_REPO=giantswarm/beekeeper gh api graphql -f query='{ viewer { login } }' -q .data.viewer.login",
	} {
		if executionDenied(h.DecideExecution(executionEvent(bashTool, map[string]any{commandKey: cmd}, workspace), executionFixture, workspace, nil)) {
			t.Errorf("read-only query was denied: %s", cmd)
		}
	}
	for _, cmd := range []string{
		"gh api graphql -f query='mutation { addComment(input: {}) { clientMutationId } }'",
		"gh api graphql -f query='  # read\nMutation { addComment(input: {}) { clientMutationId } }'",
		"gh api graphql -f query='subscription { x }'",
		"gh api graphql --field=query='mutation { x }'",
		`gh api graphql -f query="$Q"`,
		"gh api graphql -f query=$(cat q.graphql)",
		"gh api graphql -F query=@request.json",
		"gh api graphql --input request.json",
		"gh api graphql -f query='{ a }' -F n=@secret.txt",
		"gh api graphql -f query='{ a }' -f query='mutation { b }'",
		"gh api graphql -X PUT -f query='{ a }'",
		"gh api graphql",
		"gh api repos/giantswarm/beekeeper/issues -f title=fixture",
	} {
		if !executionDenied(h.DecideExecution(executionEvent(bashTool, map[string]any{commandKey: cmd}, workspace), executionFixture, workspace, nil)) {
			t.Errorf("command was not denied: %s", cmd)
		}
	}
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
		"beekeeper run -- gh pr merge 7 --repo giantswarm/beekeeper",
		"beekeeper --request-id fixture run --class build -- env gh pr merge 7 --repo giantswarm/beekeeper",
		"beekeeper sandbox scratch -- gh pr merge 7 --repo giantswarm/beekeeper",
		"beekeeper --request-id fixture sandbox scratch -- env gh pr merge 7 --repo giantswarm/beekeeper",
		"beekeeper run -- systemctl --user stop personal.service",
		"beekeeper --config /srv/workspace/other.yaml agents idle",
		"beekeeper agents execution-run fixture",
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

func TestExecutionMCPGuardOnlyAdmitsCoordinator(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		h := Hook{ExecutionMCP: enabled}
		for _, tool := range []string{"mcp__coordinator__fixture__echo", "mcp__other__echo"} {
			wantDenied := !enabled || tool == "mcp__other__echo"
			raw := executionEvent(tool, map[string]any{}, workspace)
			if got := executionDenied(h.DecideExecution(raw, executionFixture, workspace, nil)); got != wantDenied {
				t.Fatalf("claude tool %s enabled %v: %s", tool, enabled, h.DecideExecution(raw, executionFixture, workspace, nil))
			}
			raw = executionCodexEvent(tool, map[string]any{}, executionThread, workspace)
			if got := executionCodexDenied(t, h.DecideExecutionCodex(raw, executionThread, workspace, nil)); got != wantDenied {
				t.Fatalf("codex tool %s enabled %v: %s", tool, enabled, h.DecideExecutionCodex(raw, executionThread, workspace, nil))
			}
		}
	}
}
