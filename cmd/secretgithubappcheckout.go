package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// checkGitHubAppCheckout checks the repository containing the target, including
// when the target or its parent directories have not been created yet.
func checkGitHubAppCheckout(ctx context.Context, file string) error {
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return refused("cannot locate clone for %q; run git -C <clone> status", file)
		}
		dir = parent
	}
	git := func(args ...string) ([]byte, error) {
		c := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git operations in the configured target directory
		return c.Output()
	}
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return refused("target %q is not in a Git clone; configure apps.secrets to use a clone of origin/main", file)
	}
	dir = strings.TrimSpace(string(root))
	if _, err := git("fetch", "origin", "+refs/heads/main:refs/remotes/origin/main"); err != nil {
		return refused("clone %q: fetch failed; run git -C %q fetch origin", dir, dir)
	}
	status, err := git("status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return refused("clone %q: cannot check local changes; run git -C %q status", dir, dir)
	}
	if len(status) != 0 {
		return refused("clone %q has local changes; run git -C %q status and commit or stash changes (including untracked files) before retrying", dir, dir)
	}
	if _, err := git("merge-base", "--is-ancestor", "origin/main", "HEAD"); err != nil {
		return refused("clone %q is behind or diverged from origin/main; run git -C %q merge --ff-only origin/main (resolve any divergence) before retrying", dir, dir)
	}
	return nil
}
