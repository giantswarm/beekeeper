package execution

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerGitHubDispatchPreservesClientArguments(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "fixture beekeeper")
	config := filepath.Join(root, "host's config.yaml")
	// The actual dispatch script must select its config without inserting
	// root flags into a command that deliberately disables flag parsing.
	script := "#!/bin/sh\n[ \"$BEEKEEPER_CONFIG\" = \"$EXPECTED_CONFIG\" ] || exit 9\nprintf '%s\\n' \"$@\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil { //nolint:gosec // test-owned executable fixture
		t.Fatal(err)
	}
	s := &Server{Root: root, Executable: binary, ConfigPath: config}
	j := Journal{Request: StartRequest{GitHub: true}, Handle: Handle{Ref: Ref{Execution: "fixture"}}}
	if _, err := s.GitHubEnvironment(j, []string{workerGitHubTestPath}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gh", GitHubGitTool} {
		verb, args := "github-cli", []string{workerGitHubIssueGroup, workerGitHubView, "1", workerGitHubRepoFlag, workerGitHubPrivateRepo}
		if name == GitHubGitTool {
			verb, args = "github-git", []string{"push", "origin", "HEAD:refs/heads/beekeeper-execution/fixture/1"}
		}
		c := exec.Command(filepath.Join(root, workerGitHubBinDir, "fixture", name), args...) //nolint:gosec // test-owned generated adapter
		c.Env = append(os.Environ(), "BEEKEEPER_CONFIG=unrelated-fixture", "EXPECTED_CONFIG="+config)
		output, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("%s adapter failed: %v, %s", name, err, output)
		}
		want := strings.Join(append([]string{"agents", verb, "--"}, args...), "\n") + "\n"
		if string(output) != want {
			t.Fatalf("%s client argv changed: got %q, want %q", name, output, want)
		}
	}
}

const (
	workerGitHubMainBranch  = "main"
	workerGitHubPrivateRepo = "example/private"
	workerGitHubTestPath    = "PATH=/usr/bin"
	workerGitHubTestDir     = "/tmp/fixture"
	workerGitHubDefaultRepo = "example/default"
	workerGitHubOutsideRepo = "outside/project"
	workerGitHubDelete      = "delete"
	workerGitHubBodyFlag    = "--body"
	workerGitHubTitleField  = "title=fixture"
	workerGitHubTag         = "v1.0.0"
)

func TestWorkerGitHubPushScope(t *testing.T) {
	branch := "beekeeper-execution/fixture/1"
	if err := ValidateWorkerPush([]string{workerGitHubPush, GitHubOrigin, workerGitHubHeadPrefix + branch}, branch); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{workerGitHubPush}, {workerGitHubPush, GitHubOrigin, workerGitHubMainBranch}, {workerGitHubPush, "--force", GitHubOrigin, workerGitHubHeadPrefix + branch}, {workerGitHubPush, GitHubOrigin, workerGitHubHeadPrefix + branch, "HEAD:refs/heads/main"}, {workerGitHubPush, "https://other.example/repo", workerGitHubHeadPrefix + branch}} {
		if ValidateWorkerPush(args, branch) == nil {
			t.Errorf("unsafe push accepted: %v", args)
		}
	}
}

func TestWorkerGitHubRepositorySelection(t *testing.T) {
	for _, args := range [][]string{{workerGitHubIssueGroup, workerGitHubView, "1", workerGitHubRepoFlag, workerGitHubPrivateRepo}, {"pr", workerGitHubView, "https://github.com/example/private/pull/1"}, {"api", "repos/example/private/actions/runs"}} {
		repo, err := GitHubRepository(args, workerGitHubDefaultRepo)
		if err != nil || repo != workerGitHubPrivateRepo {
			t.Fatalf("scope %q err %v", repo, err)
		}
	}
	if _, err := GitHubRepository([]string{workerGitHubIssueGroup, workerGitHubView, "1", workerGitHubRepoFlag, "outside/repo/extra"}, workerGitHubDefaultRepo); err == nil {
		t.Fatal("invalid repository accepted")
	}
}

func TestWorkerGitHubWriteRepositoryResolvesEveryTarget(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{workerGitHubIssueGroup, workerGitHubCreate, "--title", mcpFixtureName}, workerGitHubDefaultRepo},
		{[]string{workerGitHubIssueGroup, workerGitHubClose, "1", workerGitHubRepoFlag, workerGitHubPrivateRepo}, workerGitHubPrivateRepo},
		{[]string{workerGitHubPRGroup, workerGitHubComment, "https://github.com/Example/Private/pull/1", "--repo=example/private"}, workerGitHubPrivateRepo},
		{[]string{workerGitHubIssueGroup, workerGitHubComment, "1", workerGitHubBodyFlag, "repos/outside/project"}, workerGitHubDefaultRepo},
		{[]string{workerGitHubIssueGroup, workerGitHubComment, "1", workerGitHubBodyFlag, "https://example.invalid/outside/project"}, workerGitHubDefaultRepo},
	} {
		if repo, err := GitHubWriteRepository(tc.args, workerGitHubDefaultRepo); err != nil || repo != tc.want {
			t.Errorf("%v: scope %q err %v", tc.args, repo, err)
		}
	}
	for _, args := range [][]string{
		{workerGitHubIssueGroup, workerGitHubComment, "1", workerGitHubRepoFlag, workerGitHubOutsideRepo, workerGitHubBodyFlag, "https://github.com/example/private/issues/1"},
		{workerGitHubIssueGroup, workerGitHubClose, "https://github.com/outside/project/issues/1", workerGitHubRepoFlag, workerGitHubPrivateRepo},
		{workerGitHubIssueGroup, workerGitHubClose, "http://github.com/outside/project/issues/1", workerGitHubRepoFlag, workerGitHubPrivateRepo},
		{workerGitHubIssueGroup, workerGitHubClose, "HTTPS://WWW.GITHUB.COM/outside/project/issues/1", workerGitHubRepoFlag, workerGitHubPrivateRepo},
		{workerGitHubIssueGroup, workerGitHubEdit, "https://github.com/example/private/issues/1", "https://github.com/outside/project/issues/2"},
		{workerGitHubIssueGroup, workerGitHubClose, "1", "-Routside/project"},
		{workerGitHubIssueGroup, workerGitHubClose, "1", "-R=outside/project"},
		{workerGitHubPRGroup, workerGitHubCreate, "-dR", workerGitHubOutsideRepo},
		{workerGitHubIssueGroup, workerGitHubClose, "1", workerGitHubRepoPrefix},
		{workerGitHubIssueGroup, workerGitHubClose, "1", workerGitHubRepoFlag},
	} {
		if repo, err := GitHubWriteRepository(args, workerGitHubPrivateRepo); err == nil {
			t.Errorf("%v: ambiguous write scoped to %q", args, repo)
		}
	}
}

func TestWorkerGitHubWriteClassification(t *testing.T) {
	for _, args := range [][]string{{workerGitHubPRGroup, "review", "1"}, {workerGitHubIssueGroup, workerGitHubCreate}, {workerGitHubIssueGroup, workerGitHubClose, "1"}, {workerGitHubIssueGroup, "reopen", "1"}, {workerGitHubIssueGroup, workerGitHubEdit, "1"}, {workerGitHubPRGroup, workerGitHubComment, "1"}} {
		if write, err := GitHubWrite(args); err != nil || !write {
			t.Errorf("%v: write %v err %v", args, write, err)
		}
	}
	for _, args := range [][]string{{workerGitHubIssueGroup, workerGitHubView, "1"}, {workerGitHubPRGroup, "checks", "1"}, {workerGitHubRunGroup, "watch", "1"}, {workerGitHubRepoGroup, workerGitHubClone, "example/private"}, {workerGitHubReleaseGroup, workerGitHubDownload, workerGitHubTag}, {workerGitHubWorkflowGroup, workerGitHubList}} {
		if write, err := GitHubWrite(args); err != nil || write {
			t.Errorf("%v: write %v err %v", args, write, err)
		}
	}
	for _, args := range [][]string{
		{workerGitHubIssueGroup}, {workerGitHubIssueGroup, workerGitHubRepoFlag, workerGitHubOutsideRepo, workerGitHubClose, "1"},
		{workerGitHubIssueGroup, workerGitHubDelete, "1"}, {workerGitHubIssueGroup, "transfer", "1", workerGitHubOutsideRepo}, {workerGitHubIssueGroup, "new"},
		{workerGitHubPRGroup, workerGitHubClose, "1"}, {workerGitHubPRGroup, "update-branch", "1"},
		{workerGitHubRepoGroup, workerGitHubDelete, workerGitHubOutsideRepo}, {workerGitHubRepoGroup, workerGitHubEdit, workerGitHubOutsideRepo}, {workerGitHubRepoGroup, "deploy-key", fixtureAddCommand},
		{workerGitHubRunGroup, "cancel", "1"}, {workerGitHubRunGroup, "rerun", "1"}, {workerGitHubRunGroup, workerGitHubDelete, "1"},
		{workerGitHubWorkflowGroup, workerGitHubRunGroup, mcpFixtureName}, {workerGitHubWorkflowGroup, "enable", mcpFixtureName}, {workerGitHubWorkflowGroup, "disable", mcpFixtureName},
		{workerGitHubReleaseGroup, workerGitHubCreate, workerGitHubTag}, {workerGitHubReleaseGroup, workerGitHubDelete, workerGitHubTag}, {workerGitHubReleaseGroup, "delete-asset", workerGitHubTag, mcpFixtureName}, {workerGitHubReleaseGroup, workerGitHubEdit, workerGitHubTag}, {workerGitHubReleaseGroup, "upload", workerGitHubTag, mcpFixtureName},
	} {
		if write, err := GitHubWrite(args); err == nil {
			t.Errorf("%v: unsupported command dispatched, write %v", args, write)
		}
	}
}

func TestWorkerGitHubAPIDispatchIsReadOnly(t *testing.T) {
	const endpoint = "repos/example/private/issues"
	for _, args := range [][]string{
		{endpoint},
		{endpoint, "--paginate", "--jq", ".[].title"},
		{"graphql", "-f", "query=query { viewer { login } }"},
		{endpoint, "-q", ".[].title", "-i"},
		{endpoint, "-iq.[].title"},
		{"-X", workerGitHubGet, endpoint, "-f", "state=open"},
		{"--method=get", endpoint, "-F", "per_page=100"},
		{"-XHEAD", endpoint},
		{"-H", "Accept: application/vnd.github+json", endpoint},
		{"--", endpoint},
	} {
		if err := ValidateGitHubAPIRead(args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{
		{},
		{"-X", "PATCH", endpoint},
		{"-Xpatch", endpoint},
		{"-X=PATCH", endpoint},
		{"-iXPATCH", endpoint},
		{"--method", "POST", endpoint},
		{"--method=PATCH", endpoint},
		{"-X", workerGitHubGet, "-X", "DELETE", endpoint},
		{"-X"},
		{endpoint, "-f", workerGitHubTitleField},
		{endpoint, "-ftitle=fixture"},
		{endpoint, "-F", workerGitHubTitleField},
		{endpoint, "--raw-field=title=fixture"},
		{endpoint, "--field", workerGitHubTitleField},
		{endpoint, "-if", workerGitHubTitleField},
		{"graphql", "-f", "query=mutation { addComment(input: {}) { clientMutationId } }"},
		{"graphql", "-F", "query=@request.graphql"},
		{"graphql", "-H", "X-HTTP-Method-Override: PUT", "-f", "query={ viewer { login } }"},
		{endpoint, "--input", "fixture.json"},
		{endpoint, "--input=-"},
		{endpoint, "--hostname", "example.invalid"},
		{endpoint, "-H", "X-HTTP-Method-Override: PATCH"},
		{endpoint, "--unknown"},
		{endpoint, "second"},
	} {
		if err := ValidateGitHubAPIRead(args); err == nil {
			t.Errorf("%v: mutation dispatched as a read", args)
		}
	}
}

func TestWorkerGitHubEnvironmentContainsDispatchOnly(t *testing.T) {
	s := &Server{Root: t.TempDir(), Executable: "/usr/bin/beekeeper", ConfigPath: "/etc/beekeeper/host.yaml"}
	j := Journal{Request: StartRequest{GitHub: true}, Handle: Handle{Ref: Ref{Execution: mcpFixtureName}}}
	env, err := s.GitHubEnvironment(j, []string{workerGitHubTestPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || !strings.Contains(env[0], workerGitHubBinDir) {
		t.Fatal("dispatch directory absent")
	}
	for _, name := range []string{"gh", GitHubGitTool} {
		p := filepath.Join(s.Root, workerGitHubBinDir, mcpFixtureName, name)
		b, err := os.ReadFile(p) //nolint:gosec // generated dispatch script in the test-owned temporary directory
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "BEEKEEPER_CONFIG=") || !strings.Contains(string(b), "agents github-") || strings.Contains(string(b), " --config ") || strings.Contains(string(b), "GH_TOKEN=") {
			t.Fatal("dispatch script embeds authentication or leaks root flags into DisableFlagParsing commands")
		}
		info, _ := os.Stat(p)
		if info.Mode().Perm() != 0o700 {
			t.Fatal("dispatch permissions")
		}
	}
}

func TestWorkerGitHubGitGlobalOptionsPreserveVerb(t *testing.T) {
	for _, args := range [][]string{{"-C", workerGitHubTestDir, workerGitHubStatus}, {"-c", "user.name=Fixture", workerGitHubStatus}, {"--git-dir=/tmp/fixture/.git", workerGitHubStatus}} {
		if got := GitHubGitVerb(args); got != workerGitHubStatus {
			t.Fatalf("local git verb %q", got)
		}
	}
	for _, args := range [][]string{{"-c", "credential.helper=other", workerGitHubPush, GitHubOrigin, workerGitHubMainBranch}, {"-C", workerGitHubTestDir, workerGitHubFetch, GitHubOrigin}} {
		if got := GitHubGitVerb(args); got != workerGitHubPush && got != workerGitHubFetch {
			t.Fatalf("network override escaped detection: %q", got)
		}
	}
}

func TestWorkerGitHubReadTransportScope(t *testing.T) {
	for _, args := range [][]string{{workerGitHubClone, "https://github.com/example/private.git", "copy"}, {"ls-remote", "git@github.com:example/private.git"}, {workerGitHubFetch, "https://github.com/example/private", workerGitHubMainBranch}} {
		repo, normalized, ok := GitHubReadRepository(args)
		if !ok || repo != workerGitHubPrivateRepo || !strings.Contains(strings.Join(normalized, " "), "https://github.com/example/private.git") {
			t.Fatal("read transport did not select canonical repository scope")
		}
	}
	if _, _, ok := GitHubReadRepository([]string{workerGitHubClone, workerGitHubTestDir, "copy"}); ok {
		t.Fatal("local clone requested GitHub authority")
	}
}
