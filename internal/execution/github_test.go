package execution

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	workerGitHubView        = "view"
	workerGitHubStatus      = "status"
	workerGitHubMainBranch  = "main"
	workerGitHubPrivateRepo = "example/private"
	workerGitHubTestPath    = "PATH=/usr/bin"
	workerGitHubTestDir     = "/tmp/fixture"
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
	for _, args := range [][]string{{"issue", workerGitHubView, "1", workerGitHubRepoFlag, workerGitHubPrivateRepo}, {"pr", workerGitHubView, "https://github.com/example/private/pull/1"}, {"api", "repos/example/private/actions/runs"}} {
		repo, err := GitHubRepository(args, "example/default")
		if err != nil || repo != workerGitHubPrivateRepo {
			t.Fatalf("scope %q err %v", repo, err)
		}
	}
	if _, err := GitHubRepository([]string{"issue", workerGitHubView, "1", workerGitHubRepoFlag, "outside/repo/extra"}, "example/default"); err == nil {
		t.Fatal("invalid repository accepted")
	}
}

func TestWorkerGitHubEnvironmentContainsDispatchOnly(t *testing.T) {
	s := &Server{Root: t.TempDir(), Executable: "/usr/bin/beekeeper", ConfigPath: "/etc/beekeeper/host.yaml"}
	j := Journal{Request: StartRequest{GitHub: true}, Handle: Handle{Ref: Ref{Execution: "fixture"}}}
	env, err := s.GitHubEnvironment(j, []string{workerGitHubTestPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || !strings.Contains(env[0], workerGitHubBinDir) {
		t.Fatal("dispatch directory absent")
	}
	for _, name := range []string{"gh", GitHubGitTool} {
		p := filepath.Join(s.Root, workerGitHubBinDir, "fixture", name)
		b, err := os.ReadFile(p) //nolint:gosec // generated dispatch script in the test-owned temporary directory
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "agents github-") || strings.Contains(string(b), "GH_TOKEN=") {
			t.Fatal("dispatch script embeds authentication")
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
