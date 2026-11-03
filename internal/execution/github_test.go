package execution

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerGitHubPushScope(t *testing.T) {
	branch := "beekeeper-execution/fixture/1"
	if err := ValidateWorkerPush([]string{"push", "origin", "HEAD:refs/heads/" + branch}, branch); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"push"}, {"push", "origin", "main"}, {"push", "--force", "origin", "HEAD:refs/heads/" + branch}, {"push", "origin", "HEAD:refs/heads/" + branch, "HEAD:refs/heads/main"}, {"push", "https://other.example/repo", "HEAD:refs/heads/" + branch}} {
		if ValidateWorkerPush(args, branch) == nil {
			t.Errorf("unsafe push accepted: %v", args)
		}
	}
}

func TestWorkerGitHubRepositorySelection(t *testing.T) {
	for _, args := range [][]string{{"issue", "view", "1", "--repo", "example/private"}, {"pr", "view", "https://github.com/example/private/pull/1"}, {"api", "repos/example/private/actions/runs"}} {
		repo, err := GitHubRepository(args, "example/default")
		if err != nil || repo != "example/private" {
			t.Fatalf("scope %q err %v", repo, err)
		}
	}
	if _, err := GitHubRepository([]string{"issue", "view", "1", "--repo", "outside/repo/extra"}, "example/default"); err == nil {
		t.Fatal("invalid repository accepted")
	}
}

func TestWorkerGitHubEnvironmentContainsDispatchOnly(t *testing.T) {
	s := &Server{Root: t.TempDir(), Executable: "/usr/bin/beekeeper", ConfigPath: "/etc/beekeeper/host.yaml"}
	j := Journal{Request: StartRequest{GitHub: true}, Handle: Handle{Ref: Ref{Execution: "fixture"}}}
	env, err := s.GitHubEnvironment(j, []string{"PATH=/usr/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || !strings.Contains(env[0], "github-bin") {
		t.Fatal("dispatch directory absent")
	}
	for _, name := range []string{"gh", "git"} {
		p := filepath.Join(s.Root, "github-bin", "fixture", name)
		b, err := os.ReadFile(p)
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
	for _, args := range [][]string{{"-C", "/tmp/fixture", "status"}, {"-c", "user.name=Fixture", "status"}, {"--git-dir=/tmp/fixture/.git", "status"}} {
		if got := GitHubGitVerb(args); got != "status" {
			t.Fatalf("local git verb %q", got)
		}
	}
	for _, args := range [][]string{{"-c", "credential.helper=other", "push", "origin", "main"}, {"-C", "/tmp/fixture", "fetch", "origin"}} {
		if got := GitHubGitVerb(args); got != "push" && got != "fetch" {
			t.Fatalf("network override escaped detection: %q", got)
		}
	}
}

func TestWorkerGitHubReadTransportScope(t *testing.T) {
	for _, args := range [][]string{{"clone", "https://github.com/example/private.git", "copy"}, {"ls-remote", "git@github.com:example/private.git"}, {"fetch", "https://github.com/example/private", "main"}} {
		repo, normalized, ok := GitHubReadRepository(args)
		if !ok || repo != "example/private" || !strings.Contains(strings.Join(normalized, " "), "https://github.com/example/private.git") {
			t.Fatal("read transport did not select canonical repository scope")
		}
	}
	if _, _, ok := GitHubReadRepository([]string{"clone", "/tmp/fixture", "copy"}); ok {
		t.Fatal("local clone requested GitHub authority")
	}
}
