//go:build unix

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
)

func TestWorkerGitHubHostCLIUsesEphemeralCredential(t *testing.T) {
	a, r := workerGitHubTestApp(t)
	root, rootErr := filepath.EvalSymlinks(t.TempDir())
	if rootErr != nil {
		t.Fatal(rootErr)
	}
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	key, err := execution.SigningKey(filepath.Join(root, "key"))
	if err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(root, "gh")
	script := "#!/bin/sh\n[ \"$GH_TOKEN\" = '" + workerTokenFixture + "' ] || exit 7\n[ \"$GH_REPO\" = 'example/private' ] || exit 8\nprintf 'private issue proof\\n'\n"
	if err := os.WriteFile(cli, []byte(script), 0o700); err != nil { //nolint:gosec // executable fixture in this test's temporary directory
		t.Fatal(err)
	}
	s := &execution.Server{Root: filepath.Join(root, "execution"), Rules: []byte("fixture rules"), Version: remoteMCPFixture, Executable: sshTestExecutable, ConfigPath: sshTestConfig, Config: execution.HostConfig{Identity: r.Ref.Host, RulesFile: filepath.Join(root, "rules.md"), WorkspaceRoot: filepath.Join(root, "workspaces"), Repositories: map[string]string{r.Repository: workspace}, Harnesses: map[string]execution.HarnessConfig{execution.HarnessClaude: {Executable: hostFixtureClaude, Version: publicationClaudeVersion}}, Tools: map[string]execution.ToolConfig{execution.ToolGitHub: {Executable: cli, Version: "2.80.0"}}, Coordinators: map[string]execution.Coordinator{r.Coordinator: {PublicKey: execution.PublicKey(key), Callback: execution.Endpoint{SSH: hostFixtureCoordinator, Executable: sshTestExecutable, Config: sshTestConfig}}}, MaxBusy: 1}}
	digest, err := s.Prepare(s.Policy())
	if err != nil {
		t.Fatal(err)
	}
	r.PolicyDigest = digest
	j := execution.Journal{Request: r, Digest: r.Digest(), Handle: execution.Handle{Ref: r.Ref, Unit: execution.UnitName(r.Ref), ConversationID: r.ConversationID, Workspace: workspace}}
	raw, _ := json.Marshal(j)
	if err := os.WriteFile(filepath.Join(s.Root, string(r.Ref.Execution)+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s.OwnUnit = func() string { return j.Handle.Unit }
	calls := 0
	wantRepo, wantWrite := workerGitHubPrivateRepo, false
	s.Callback = func(_ context.Context, _ []string, raw []byte) ([]byte, error) {
		var req execution.CallbackRequest
		if err := execution.Decode(raw, &req); err != nil {
			t.Fatal(err)
		}
		var scope workerGitHubRequest
		if req.Operation != workerGitHubIssue || !execution.VerifyCallback(req, r.ControlKey) || execution.Decode(req.Payload, &scope) != nil || scope.Repository != wantRepo || scope.Write != wantWrite {
			t.Fatal("wrong host callback scope")
		}
		calls++
		return json.Marshal(execution.WireResponse{Protocol: execution.Protocol, RequestID: req.RequestID, Ref: r.Ref, Payload: mustExecutionJSON(t, workerGitHubToken{Token: workerTokenFixture, Expires: time.Now().Add(time.Hour)})})
	}
	a.executionRuntime = func() (*execution.Server, error) { return s, nil }
	t.Setenv(execution.EnvID, string(r.Ref.Execution))
	t.Setenv("GH_TOKEN", "unrelated-ambient-fixture")
	var output bytes.Buffer
	for _, args := range [][]string{{"issue", "view", "1", loopRepoFlag, workerGitHubPrivateRepo}, {"pr", "view", "2", loopRepoFlag, workerGitHubPrivateRepo}, {"pr", "checks", "2", loopRepoFlag, workerGitHubPrivateRepo}} {
		c := a.executionGitHubCommands()[0]
		c.SetOut(&output)
		c.SetErr(&output)
		c.SetArgs(args)
		c.SilenceErrors, c.SilenceUsage = true, true
		if err := c.ExecuteContext(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 || strings.Contains(output.String(), workerTokenFixture) || strings.Count(output.String(), "private issue proof") != 3 {
		t.Fatal("host CLI proof failed or leaked credential")
	}
	c := a.executionGitHubCommands()[0]
	c.SetArgs([]string{"pr", gitMerge, "2"})
	c.SilenceErrors, c.SilenceUsage = true, true
	if c.ExecuteContext(context.Background()) == nil || calls != 3 {
		t.Fatal("merge reached token issuer")
	}
	// The same dispatch path invokes git on the host, with the owned branch
	// and token in its environment, while local commands never ask for one.
	git := filepath.Join(root, "git")
	gitScript := "#!/bin/sh\nif [ \"$1\" = status ]; then [ -z \"$GH_TOKEN\" ] || exit 9; printf 'local git proof\\n'; exit 0; fi\n[ \"$GH_TOKEN\" = '" + workerTokenFixture + "' ] || exit 7\nprintf 'host push proof\\n'\n"
	if err := os.WriteFile(git, []byte(gitScript), 0o700); err != nil { //nolint:gosec // executable fixture in this test's temporary directory
		t.Fatal(err)
	}
	previousGit := workerGitHubGitExecutable
	workerGitHubGitExecutable = git
	t.Cleanup(func() { workerGitHubGitExecutable = previousGit })
	t.Setenv("GH_TOKEN", "")
	wantRepo, wantWrite = r.Repository, true
	gitCommand := a.executionGitHubCommands()[1]
	gitCommand.SetArgs([]string{workerGitHubPush, workerGitHubOrigin, "HEAD:refs/heads/main"})
	gitCommand.SetOut(&output)
	gitCommand.SetErr(&output)
	gitCommand.SilenceErrors, gitCommand.SilenceUsage = true, true
	if gitCommand.ExecuteContext(context.Background()) == nil || calls != 3 {
		t.Fatal("foreign branch push reached issuer")
	}
	gitCommand = a.executionGitHubCommands()[1]
	gitCommand.SetArgs([]string{workerGitHubPush, workerGitHubOrigin, "HEAD:refs/heads/" + execution.GitHubBranch(r.Ref)})
	gitCommand.SetOut(&output)
	gitCommand.SetErr(&output)
	if err := gitCommand.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	gitCommand = a.executionGitHubCommands()[1]
	gitCommand.SetArgs([]string{executionStatus})
	gitCommand.SetOut(&output)
	gitCommand.SetErr(&output)
	if err := gitCommand.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 4 || !strings.Contains(output.String(), "host push proof") || !strings.Contains(output.String(), "local git proof") {
		t.Fatal("git did not separate local and credentialed commands")
	}

}

func TestWorkerGitHubGitCredentialHelperDoesNotEvaluateToken(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	const fixture = "fixture-$not_a_command-'quoted'"
	command := exec.Command(git, "-c", "credential.helper=", "-c", workerGitHubCredentialHelper, "credential", "fill") //nolint:gosec // resolved system git and a fixed fixture-only credential operation
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "GH_TOKEN=" + fixture, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
	command.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=example/repo.git\n\n")
	out, err := command.Output()
	if err != nil {
		t.Fatal("credential helper failed")
	}
	if !strings.Contains(string(out), "password="+fixture+"\n") {
		t.Fatal("credential helper changed the opaque credential")
	}
}
