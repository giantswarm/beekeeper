package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/giantswarm/beekeeper/internal/execution"
)

// An agy worker runs under a managed HOME without Git configuration while the
// execution user's home carries the identity: the base merge of
// agents publish --sync-base takes it from there, as one from claude does.
func TestPublicationIdentityReadsExecutionHome(t *testing.T) {
	managed, user, workspace := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(user, ".gitconfig"), []byte("[user]\n\tname = Execution User\n\temail = execution@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", managed)
	t.Setenv("GIT_CONFIG_COUNT", "0")
	for _, k := range []string{"GIT_CONFIG_GLOBAL", "XDG_CONFIG_HOME", "EMAIL"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("git", "init", "-q", workspace).CombinedOutput(); err != nil { //nolint:gosec // fixed git init of the test's own temporary directory
		t.Fatalf("git init: %v: %s", err, out)
	}
	want := []string{
		"GIT_AUTHOR_NAME=Execution User", "GIT_AUTHOR_EMAIL=execution@example.invalid",
		"GIT_COMMITTER_NAME=Execution User", "GIT_COMMITTER_EMAIL=execution@example.invalid",
	}

	// A workspace prepared before it carried an identity still merges: no
	// host identity falls back to the prepared workspace's.
	t.Setenv(execution.EnvHome, "")
	got, err := publicationIdentity(t.Context(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if fallback := []string{
		"GIT_AUTHOR_NAME=" + execution.WorkerGitName, "GIT_AUTHOR_EMAIL=" + execution.WorkerGitEmail,
		"GIT_COMMITTER_NAME=" + execution.WorkerGitName, "GIT_COMMITTER_EMAIL=" + execution.WorkerGitEmail,
	}; !slices.Equal(got, fallback) {
		t.Fatalf("without an execution home identity = %v, want %v", got, fallback)
	}

	t.Setenv(execution.EnvHome, user)
	got, err = publicationIdentity(t.Context(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("identity = %v, want %v", got, want)
	}
}
