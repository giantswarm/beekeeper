package worktrees

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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
	var tips []string
	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		found = found || fields[1] == "refs/heads/"+t.HeadBranch()
		if fields[0] == t.Head {
			published = true
		}
		tips = append(tips, fields[0])
	}
	if !published && len(tips) > 0 {
		// Ask once whether HEAD has any commit outside all advertised tips.
		// Missing remote objects are ignored, never treated as publication.
		// HEAD came from Inspect and must exist locally.
		slices.Sort(tips)
		tips = slices.Compact(tips)
		args := append([]string{"rev-list", "--max-count=1", "--ignore-missing", t.Head, "--not"}, tips...)
		remaining, err := git(ctx, t.Dir, args...)
		published = err == nil && len(strings.TrimSpace(string(remaining))) == 0
	}
	trackedBranch := t.Pushed != "" && t.Pushed == t.HeadBranch()
	return !found && (trackedBranch || hadPull), published, nil
}

// Automatic judges a worktree for the disk step. A harness worktree whose
// complete work is pushed can be removed without a pull request. Other
// worktrees need a merged pull request or a confirmed deleted remote branch.
// Unlike the legacy doctor trim, unfinished tasks keep their build output.
func Automatic(t Tree, pull *github.HeadPull, gone, harness, live, pushed bool, now time.Time) Verdict {
	return AutomaticPressure(t, pull, gone, harness, live, pushed, now, false)
}

// AutomaticPressure expires retention grace only for tasks confirmed ended.
// Live processes, locks, open tasks and unpublished work retain their protections.
func AutomaticPressure(t Tree, pull *github.HeadPull, gone, harness, live, pushed bool, now time.Time, pressure bool) Verdict {
	ended := gone || pull != nil && pull.State == github.Merged
	if pressure && ended {
		t.Touched = now.Add(-Grace)
		if pull != nil {
			p := *pull
			p.ClosedAt = now.Add(-Grace)
			pull = &p
		}
	}
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

// Containing returns the linked worktree dir lies in, at or above it: the
// nearest .git decides, so a checkout inside a worktree is not that worktree.
// It is "" for none; an error says that could not be told, and a caller
// protecting work treats that as a worktree.
func Containing(dir string) (string, error) {
	for p := filepath.Clean(dir); ; p = filepath.Dir(p) {
		fi, err := os.Lstat(filepath.Join(p, ".git"))
		switch {
		case err == nil && fi.Mode().IsRegular():
			if _, err := os.ReadFile(filepath.Join(p, ".git")); err != nil { //nolint:gosec // a session's own folder
				return "", err
			}
			if linked(p) {
				return p, nil
			}
			return "", nil
		case err == nil:
			return "", nil
		case !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR):
			return "", err
		}
		if p == filepath.Dir(p) {
			return "", nil
		}
	}
}

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
