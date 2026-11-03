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

// RemoteStatus verifies the branch and publication against current remote
// heads. Local tracking refs and closed pull requests are not publication
// evidence: they can outlive an unmerged branch deleted from the remote.
func RemoteStatus(ctx context.Context, git Git, t Tree, hadPull bool) (gone, published bool, err error) {
	out, err := git(ctx, t.Dir, "ls-remote", "--heads", "origin")
	if err != nil {
		return false, false, err
	}
	found := false
	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		found = found || fields[1] == "refs/heads/"+t.HeadBranch()
		if fields[0] == t.Head {
			published = true
			continue
		}
		if _, err := git(ctx, t.Dir, "merge-base", "--is-ancestor", t.Head, fields[0]); err == nil {
			published = true
		}
	}
	trackedBranch := t.Pushed != "" && t.Pushed == t.HeadBranch()
	return !found && (trackedBranch || hadPull), published, nil
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
	if pull != nil {
		if pull.State == github.Open {
			return Verdict{Keep, "pull request open", KeptOpen}
		}
		if now.Sub(pull.ClosedAt) < Grace {
			return Verdict{Keep, "closed within the grace", KeptClosed}
		}
	}
	if pull == nil && (t.Pushed == "" || t.Pushed != t.HeadBranch()) {
		gone = false
	}
	merged := pull != nil && pull.State == github.Merged
	if !merged && !gone && !harness {
		return Verdict{Keep, "task not ended", KeptOpen}
	}
	why := "remote branch gone"
	if merged {
		why = "pull request merged"
	} else if harness {
		why = "unused harness worktree"
	}
	if !pushed {
		t.Unpushed = max(1, t.Unpushed)
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
