package worktrees

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const trackingRemote = "origin"

func TestInspectTrackingConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, remote, destination, source, want string
	}{
		{"standard", trackingRemote, "refs/remotes/origin/feature", "refs/heads/feature", automaticFeature},
		{"custom namespace", trackingRemote, "refs/cache/origin/feature", "refs/heads/feature", automaticFeature},
		{"other remote", "other", "refs/remotes/other/feature", "refs/heads/feature", ""},
		{"local upstream", ".", "refs/heads/feature", "refs/heads/feature", ""},
		{"no upstream", "", "refs/remotes/origin/feature", "", ""},
		{"default upstream", trackingRemote, "refs/remotes/origin/main", "refs/heads/main", automaticDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo := filepath.Join(t.TempDir(), "repo")
			git := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
				cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // the test's own repository
				return cmd.Output()
			}
			run := func(dir string, args ...string) []byte {
				t.Helper()
				out, err := git(ctx, dir, args...)
				if err != nil {
					t.Fatalf("git %v: %v", args, err)
				}
				return out
			}
			run(filepath.Dir(repo), "init", "--initial-branch=main", repo)
			run(repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "initial")
			run(repo, "remote", "add", trackingRemote, "https://github.com/example/repo.git")
			run(repo, "config", "remote.origin.fetch", "+refs/heads/*:"+filepath.ToSlash(filepath.Dir(tc.destination))+"/*")
			treeDir := filepath.Join(filepath.Dir(repo), "tree")
			run(repo, "worktree", "add", "-b", "wt/feature", treeDir)
			run(repo, "update-ref", tc.destination, "HEAD")
			if tc.remote != "" {
				run(repo, "config", "branch.wt/feature.remote", tc.remote)
				run(repo, "config", "branch.wt/feature.merge", tc.source)
			}
			for _, pruned := range []bool{false, true} {
				if pruned {
					run(repo, "update-ref", "-d", tc.destination)
				}
				tree, err := Inspect(ctx, git, treeDir)
				if err != nil {
					t.Fatal(err)
				}
				if tree.Pushed != tc.want {
					t.Fatalf("pruned=%v: Pushed=%q, want %q", pruned, tree.Pushed, tc.want)
				}
				wantHead := "wt/feature"
				if tc.want == automaticFeature {
					wantHead = tc.want
				}
				if tree.HeadBranch() != wantHead {
					t.Fatalf("HeadBranch=%q, want %q", tree.HeadBranch(), wantHead)
				}
				// Supply advertised publication without contacting a remote. The task
				// branch is absent, but its commit is published on main.
				advertised := func(_ context.Context, _ string, args ...string) ([]byte, error) {
					if len(args) != 3 || args[0] != "ls-remote" || args[1] != "--heads" || args[2] != trackingRemote {
						return nil, fmt.Errorf("unexpected git arguments: %v", args)
					}
					return []byte(tree.Head + "\trefs/heads/main\n"), nil
				}
				gone, published, err := RemoteStatus(ctx, advertised, tree, false)
				if err != nil {
					t.Fatal(err)
				}
				if gone != (tc.want == automaticFeature) || !published {
					t.Fatalf("pruned=%v: gone=%v published=%v", pruned, gone, published)
				}
				tree.Touched = time.Now().Add(-2 * Grace)
				verdict := Automatic(tree, nil, gone, false, false, published, time.Now())
				wantAction := Keep
				if tc.want == automaticFeature {
					wantAction = Remove
				}
				if verdict.Action != wantAction {
					t.Fatalf("Automatic=%v, want %s", verdict, wantAction)
				}
			}
		})
	}
}
