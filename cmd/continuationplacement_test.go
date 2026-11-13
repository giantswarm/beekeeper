package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/giantswarm/beekeeper/internal/loop"
)

// A coordinator worker's workspace keeps its continuation on the
// coordinator only while it holds work no remote has: uncommitted changes to
// tracked files or commits on no remote branch. Untracked files, a pushed
// head, a workspace that is gone and a folder that is no checkout hold none.
func TestContinuationUnpublished(t *testing.T) {
	ctx := context.Background()
	checkout := t.TempDir()
	testCheckoutArgs(t, checkout, nil)
	unpublished := func() string { return loop.ContinuationUnpublished(ctx, checkout) }
	git := func(args ...string) {
		t.Helper()
		if _, err := runGit(ctx, checkout, args...); err != nil {
			t.Fatal(err)
		}
	}
	if got := unpublished(); got != "" {
		t.Fatalf("pushed head: %q", got)
	}
	if err := os.WriteFile(filepath.Join(checkout, "scratch.md"), []byte("a draft\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := unpublished(); got != "" {
		t.Fatalf("untracked file: %q", got)
	}
	if err := os.WriteFile(filepath.Join(checkout, "fix.txt"), []byte("the fix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(loop.LoopAdd, "fix.txt")
	if got := unpublished(); got != "uncommitted changes" {
		t.Fatalf("staged change: %q", got)
	}
	if _, err := runGit(ctx, checkout, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "fix: the fix"); err != nil {
		t.Fatal(err)
	}
	if got := unpublished(); got != "a commit on no remote branch" {
		t.Fatalf("local commit: %q", got)
	}
	git("push", "--quiet", workerGitHubOrigin, "HEAD:refs/heads/fix")
	if got := unpublished(); got != "" {
		t.Fatalf("pushed commit: %q", got)
	}
	if got := loop.ContinuationUnpublished(ctx, filepath.Join(checkout, "gone")); got != "" {
		t.Fatalf("missing workspace: %q", got)
	}
	if got := loop.ContinuationUnpublished(ctx, t.TempDir()); got != "" {
		t.Fatalf("no checkout: %q", got)
	}
}
