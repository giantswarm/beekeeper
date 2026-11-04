package worktrees

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestInspectPreservesPrunedOriginUpstream(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_OPTIONAL_LOCKS", "0")
	for _, namespace := range []string{"origin", "renamed-tracking-refs"} {
		t.Run(namespace, func(t *testing.T) {
			git := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
				out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).CombinedOutput() //nolint:gosec // local fixture commands only
				if err != nil {
					return nil, fmt.Errorf("git %v: %w: %s", args, err, out)
				}
				return out, nil
			}
			run := func(dir string, args ...string) {
				t.Helper()
				if _, err := git(context.Background(), dir, args...); err != nil {
					t.Fatal(err)
				}
			}
			clone := t.TempDir()
			dir := filepath.Join(t.TempDir(), "worktree")
			run(clone, "init", "-q", "-b", "main")
			run(clone, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "base")
			run(clone, "remote", "add", "origin", filepath.Join(t.TempDir(), "origin.git"))
			run(clone, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/"+namespace+"/*")
			run(clone, "worktree", "add", "-q", "-b", "wt/feature", dir, "main")
			tracking := "refs/remotes/" + namespace + "/feature"
			run(dir, "update-ref", tracking, "HEAD")
			run(dir, "branch", "--set-upstream-to="+tracking, "wt/feature")
			for _, pruned := range []bool{false, true} {
				if pruned {
					run(dir, "update-ref", "-d", tracking)
				}
				tree, err := Inspect(context.Background(), git, dir)
				if err != nil {
					t.Fatal(err)
				}
				if tree.Pushed != "feature" || tree.HeadBranch() != "feature" {
					t.Fatalf("pruned=%v: pushed=%q head branch=%q, want feature", pruned, tree.Pushed, tree.HeadBranch())
				}
				// Advertise publication without contacting a remote. The deleted
				// feature is still identifiable after its tracking ref is pruned.
				remote := func(_ context.Context, _ string, args ...string) ([]byte, error) {
					if !slices.Equal(args, []string{"ls-remote", "--heads", "origin"}) {
						t.Fatalf("unexpected remote query: %v", args)
					}
					return []byte(tree.Head + " refs/heads/main\n"), nil
				}
				gone, published, err := RemoteStatus(context.Background(), remote, tree, false)
				if err != nil || !gone || !published {
					t.Fatalf("pruned=%v: gone=%v published=%v err=%v", pruned, gone, published, err)
				}
				now := time.Now()
				tree.Touched = now.Add(-2 * Grace)
				if got := Automatic(tree, nil, gone, false, false, published, now); got.Action != Remove {
					t.Fatalf("pruned=%v: verdict=%+v, want remove", pruned, got)
				}
			}
		})
	}
}
