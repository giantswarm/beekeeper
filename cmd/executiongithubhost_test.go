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

const workerGitHubTokenVerb = "token"
const workerGitHubRevParse = "rev-parse"
const workerGitHubMissingRef = "missing"

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
	for _, args := range [][]string{{workerGitHubIssueGroup, "view", "1", loopRepoFlag, workerGitHubPrivateRepo}, {"pr", "view", "2", loopRepoFlag, workerGitHubPrivateRepo}, {"pr", "checks", "2", loopRepoFlag, workerGitHubPrivateRepo}} {
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
	for _, args := range [][]string{{loopRepoFlag, workerGitHubPrivateRepo, "auth", workerGitHubTokenVerb}, {"unknown-alias"}} {
		denied := a.executionGitHubCommands()[0]
		denied.SetArgs(args)
		denied.SilenceErrors, denied.SilenceUsage = true, true
		if denied.ExecuteContext(context.Background()) == nil || calls != 3 {
			t.Fatal("alternate auth or alias path reached token issuer")
		}
	}

	// Reviews need a write-scoped credential for the task repository.
	wantRepo, wantWrite = r.Repository, true
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nprintf 'review posted\\n'\n"), 0o700); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
	review := a.executionGitHubCommands()[0]
	review.SetOut(&output)
	review.SetErr(&output)
	review.SetArgs([]string{"pr", taskKindReview, "2", "--comment", "--body", "Review: approved"})
	if err := review.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantRepo, wantWrite = workerGitHubPrivateRepo, false

	// The same dispatch path invokes git on the host, with the owned branch
	// and token in its environment, while local commands never ask for one.
	git := filepath.Join(root, "git")
	gitScript := "#!/bin/sh\nif [ \"$1\" = rev-parse ]; then case \"$2\" in --show-object-format) echo sha1; exit 0;; --path-format=absolute) echo /tmp/fixture; exit 0;; --verify) if [ \"$3\" = 'HEAD^{commit}' ]; then echo 0123456789012345678901234567890123456789; exit 0; fi;; esac; fi\nif [ \"$1\" = rev-parse ]; then exit 42; fi\nif [ \"$1\" = status ]; then [ -z \"$GH_TOKEN\" ] || exit 9; printf 'local git proof\\n'; exit 0; fi\n[ \"$GH_TOKEN\" = '" + workerTokenFixture + "' ] || exit 7\n" +
		"if [ \"$GH_REPO\" = 'example/private' ]; then matched=0; for arg in \"$@\"; do [ \"$arg\" = 'remote.origin.url=https://github.com/example/private.git' ] && matched=1; done; [ \"$matched\" = 1 ] || exit 10; printf 'host read proof\\n'; else printf 'host push proof\\n'; fi\n"
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
	if gitCommand.ExecuteContext(context.Background()) == nil || calls != 4 {
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
	wantRepo, wantWrite = workerGitHubPrivateRepo, false
	gitCommand = a.executionGitHubCommands()[1]
	gitCommand.SetArgs([]string{workerGitHubClone, "https://github.com/example/private.git", "copy"})
	gitCommand.SetOut(&output)
	gitCommand.SetErr(&output)
	if err := gitCommand.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	gitCommand = a.executionGitHubCommands()[1]
	gitCommand.SetArgs([]string{workerGitHubRevParse, "--verify", workerGitHubMissingRef})
	gitCommand.SilenceErrors, gitCommand.SilenceUsage = true, true
	if err := gitCommand.ExecuteContext(context.Background()); err == nil || Code(err) != 42 {
		t.Fatal("client exit status was not preserved")
	}
	if calls != 6 || !strings.Contains(output.String(), "host push proof") || !strings.Contains(output.String(), "local git proof") || !strings.Contains(output.String(), "host read proof") {
		t.Fatal("git did not separate local and scoped read/write commands")
	}

	// Exercise the authenticated dispatch with real Git. Only the test transport
	// shim rewrites the canonical GitHub URL to a disposable local destination.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	runGit := func(dir string, args ...string) string {
		t.Helper()
		c := exec.Command(realGit, args...) //nolint:gosec // real Git and test-owned local repositories
		c.Dir = dir
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	destination := filepath.Join(root, "destination.git")
	runGit(root, "init", "--bare", destination)
	runGit(workspace, "init")
	runGit(workspace, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	runGit(workspace, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "tag", "-a", "unexpected-tag", "-m", "fixture")
	runGit(workspace, "branch", "unexpected-branch")
	runGit(workspace, "config", "push.followTags", "true")
	shim := "#!/bin/sh\nexec '" + realGit + "' -c 'protocol.file.allow=always' -c 'url.file://" + destination + ".insteadOf=https://github.com/" + r.Repository + ".git' \"$@\"\n"
	if err := os.WriteFile(git, []byte(shim), 0o700); err != nil { //nolint:gosec // executable local-only Git transport fixture
		t.Fatal(err)
	}
	for _, polluted := range []bool{false, true} {
		if polluted {
			// Ambient Git settings must not redirect the source or load extra config.
			t.Setenv("GIT_DIR", filepath.Join(root, "foreign.git"))
			t.Setenv("GIT_CONFIG_COUNT", "1")
			t.Setenv("GIT_CONFIG_KEY_0", "push.followTags")
			t.Setenv("GIT_CONFIG_VALUE_0", "true")

			runGit(workspace, "config", "remote.origin.mirror", "true")
			runGit(workspace, "config", "remote.origin.receivepack", "/does-not-exist")
			runGit(workspace, "config", "--add", "remote.origin.pushurl", filepath.Join(root, "unwanted.git"))
			runGit(workspace, "config", "push.recurseSubmodules", "on-demand")
			// A failing hook proves the push did not consult workspace hooks either.
			if err := os.WriteFile(filepath.Join(workspace, ".git", "hooks", "pre-push"), []byte("#!/bin/sh\nexit 99\n"), 0o700); err != nil { //nolint:gosec // executable fixture hook in the test-owned repository
				t.Fatal(err)
			}
		}
		wantRepo, wantWrite = r.Repository, true
		push := a.executionGitHubCommands()[1]
		push.SetArgs([]string{workerGitHubPush, workerGitHubOrigin, "HEAD:refs/heads/" + execution.GitHubBranch(r.Ref)})
		push.SetOut(&output)
		push.SetErr(&output)
		if err := push.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("isolated real Git push: %v: %s", err, output.String())
		}
		refs := runGit(root, "--git-dir="+destination, "for-each-ref", "--format=%(refname)")
		if refs != "refs/heads/"+execution.GitHubBranch(r.Ref) {
			t.Fatalf("push widened authority: %s", refs)
		}
		got := runGit(root, "--git-dir="+destination, "rev-parse", "refs/heads/"+execution.GitHubBranch(r.Ref))
		if got != runGit(workspace, "rev-parse", "HEAD") {
			t.Fatal("push did not publish exactly the owned HEAD with scoped issuance")
		}
	}
	if calls != 8 {
		t.Fatal("real pushes did not request their scoped credentials")
	}

	// Secondary repository inside the owned workspace can push to its own
	// execution branch and park on its PR for review.
	secondRepo := "example/second"
	secondDest := filepath.Join(root, "second-destination.git")
	runGit(root, "init", "--bare", secondDest)
	secondCheckout := filepath.Join(workspace, ".beekeeper-scratch", "second")
	if err := os.MkdirAll(secondCheckout, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(secondCheckout, "init")
	runGit(secondCheckout, "remote", "add", "origin", "https://github.com/"+secondRepo+".git")
	runGit(secondCheckout, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "second fixture")
	secondHead := runGit(secondCheckout, "rev-parse", "HEAD")
	shimSecond := "#!/bin/sh\nexec '" + realGit + "' -c 'protocol.file.allow=always' -c 'url.file://" + secondDest + ".insteadOf=https://github.com/" + secondRepo + ".git' \"$@\"\n"
	if err := os.WriteFile(git, []byte(shimSecond), 0o700); err != nil { //nolint:gosec // executable local-only Git transport fixture
		t.Fatal(err)
	}
	t.Chdir(secondCheckout)
	wantRepo, wantWrite = secondRepo, true
	pushSecond := a.executionGitHubCommands()[1]
	pushSecond.SetArgs([]string{workerGitHubPush, workerGitHubOrigin, "HEAD:refs/heads/" + execution.GitHubBranch(r.Ref)})
	pushSecond.SetOut(&output)
	pushSecond.SetErr(&output)
	if err := pushSecond.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("secondary repo push: %v: %s", err, output.String())
	}
	if got := runGit(root, "--git-dir="+secondDest, "rev-parse", "refs/heads/"+execution.GitHubBranch(r.Ref)); got != secondHead {
		t.Fatalf("secondary repo push head = %q, want %q", got, secondHead)
	}
	prevReviewGH := executionReviewGH
	executionReviewGH = func(_ context.Context, _ ...string) ([]byte, error) {
		return json.Marshal(map[string]any{"head": map[string]any{"ref": execution.GitHubBranch(r.Ref), "sha": secondHead, "repo": map[string]any{"full_name": secondRepo}}})
	}
	t.Cleanup(func() { executionReviewGH = prevReviewGH })
	wsHead := a.executionWorkspaceHead(context.Background(), secondRepo+"#12")
	if wsHead != secondHead {
		t.Fatalf("executionWorkspaceHead = %q, want %q", wsHead, secondHead)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := executionReviewHead(context.Background(), st.Starts[0].Execution, secondRepo+"#12", wsHead); got != secondHead {
		t.Fatalf("executionReviewHead = %q, want %q", got, secondHead)
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
