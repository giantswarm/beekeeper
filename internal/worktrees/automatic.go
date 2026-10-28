package worktrees

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
)

// FindHarness finds harness worktrees in checkouts under the supplied
// places, without descending into build output or repository metadata.
func FindHarness(places []string) []string {
	var out []string
	places = slices.Clone(places)
	slices.Sort(places)
	places = slices.Compact(places)
	for _, root := range places {
		root = filepath.Clean(root)
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
				out = append(out, Find([]string{filepath.Join(p, ".claude", "worktrees")})...)
				return filepath.SkipDir
			}
			if p != root && (strings.HasPrefix(d.Name(), ".") || regenerable(d.Name()) || strings.Count(p[len(root):], string(filepath.Separator)) >= maxDepth) {
				return filepath.SkipDir
			}
			return nil
		})
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// RemoteGone asks the remote itself; stale tracking refs and a network
// failure must not be interpreted as a deleted branch.
func RemoteGone(ctx context.Context, git Git, t Tree) (bool, error) {
	out, err := git(ctx, t.Dir, "ls-remote", "--heads", "origin", "refs/heads/"+t.HeadBranch())
	return err == nil && len(strings.TrimSpace(string(out))) == 0, err
}

// Automatic judges a worktree for the disk step. A harness worktree whose
// complete work is pushed can be removed without a pull request. Other
// worktrees need a merged pull request or a confirmed deleted remote branch.
// Unlike the legacy doctor trim, unfinished tasks keep their build output.
func Automatic(t Tree, pull *github.HeadPull, gone, harness, live, pushed bool, now time.Time) Verdict {
	if live {
		return Verdict{Keep, "a live session or process works in it", KeptLive}
	}
	if t.Locked {
		return Verdict{Keep, "locked", KeptLocked}
	}
	if now.Sub(t.Touched) < Grace {
		return Verdict{Keep, "touched within the grace", KeptTouched}
	}
	merged := pull != nil && pull.State == github.Merged
	if !merged && !gone && !harness {
		return Verdict{Keep, "task not ended", KeptOpen}
	}
	if merged && now.Sub(pull.ClosedAt) < Grace {
		return Verdict{Keep, "merged within the grace", KeptClosed}
	}
	why := "remote branch gone"
	if merged {
		why = "pull request merged"
	} else if harness {
		why = "unused harness worktree"
	}
	return finished(t, why, pushed)
}

// Linked reports whether a directory is a linked checkout, including a
// dangling one; a main checkout is not eligible for automatic removal.
func Linked(dir string) bool { return linked(dir) }

// NestedCheckout protects checkouts inside a worktree, including harness
// worktrees and repositories under ignored build directories.
func NestedCheckout(dir string) (bool, error) {
	found := false
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
			if filepath.Dir(p) == dir {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found, err
}
