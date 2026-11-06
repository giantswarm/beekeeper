package worktrees

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SessionPath places a branch beside other sessions of the main checkout.
// Resolve existing parents to prevent a symlink from hiding a nested target.
func SessionPath(root, repo, branch string) (string, error) {
	if !filepath.IsAbs(root) || branch == "" || strings.HasPrefix(branch, "-") {
		return "", fmt.Errorf("worktree root must be absolute and branch must be nonempty")
	}
	for _, part := range strings.Split(filepath.ToSlash(branch), "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid worktree branch %q", branch)
		}
	}
	p := filepath.Join(root, filepath.Base(repo), branch)
	if NestedSessionPath(p) {
		return "", fmt.Errorf("projects.worktrees must be outside checkouts' .claude/worktrees")
	}
	return p, nil
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if parent := filepath.Dir(p); parent != p {
		return filepath.Join(realPath(parent), filepath.Base(p))
	}
	return p
}

// NestedSessionPath reports targets under a checkout's harness worktree area.
func NestedSessionPath(p string) bool {
	p = realPath(filepath.Clean(p))
	for d := p; filepath.Dir(d) != d; d = filepath.Dir(d) {
		if filepath.Base(d) == "worktrees" && filepath.Base(filepath.Dir(d)) == ".claude" {
			_, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(d)), ".git"))
			if err == nil {
				return true
			}
		}
	}
	return false
}

// MainCheckout resolves the common git directory even when cwd is a linked worktree.
func MainCheckout(ctx context.Context, git Git, cwd string) (string, error) {
	out, err := git(ctx, cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	common := strings.TrimSpace(string(out))
	if filepath.Base(common) != ".git" {
		return "", fmt.Errorf("expected a non-bare checkout")
	}
	return filepath.Dir(common), nil
}

// NestedSessions finds linked worktrees inside clones, without walking their copies.
func NestedSessions(projects string) []string {
	var roots []string
	if projects == "" {
		return nil
	}
	_ = filepath.WalkDir(projects, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			roots = append(roots, filepath.Join(p, ".claude", "worktrees"))
			return filepath.SkipDir
		}
		if p != projects && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		return nil
	})
	return Find(roots)
}

// SessionRemovable requires a clean, unlocked tree whose complete change is
// already integrated in the default branch, including a squash merge.
// Failed or conflicting merge simulations conservatively retain the tree.
func SessionRemovable(ctx context.Context, git Git, dir string) (bool, error) {
	status, err := git(ctx, dir, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	if len(status) != 0 {
		return false, nil
	}
	gd, err := git(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(filepath.Join(strings.TrimSpace(string(gd)), "locked")); err == nil {
		return false, nil
	}
	ref := ""
	for _, candidate := range []string{"refs/remotes/origin/HEAD", "refs/heads/main", "refs/heads/master"} {
		if _, err := git(ctx, dir, "rev-parse", "--verify", candidate); err == nil {
			ref = candidate
			break
		}
	}
	if ref == "" {
		return false, nil
	}
	if _, err := git(ctx, dir, "merge-base", "--is-ancestor", "HEAD", ref); err == nil {
		return true, nil
	}
	merged, err := git(ctx, dir, "merge-tree", "--write-tree", ref, "HEAD")
	if err != nil {
		return false, nil
	}
	tree, err := git(ctx, dir, "rev-parse", ref+"^{tree}")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(merged)) == strings.TrimSpace(string(tree)), nil
}
