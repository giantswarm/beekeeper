// Package worktrees finds the linked git worktrees under the configured
// roots and tells a finished one (its pull request merged or closed) from
// one that still holds work. A finished worktree that holds nothing but
// build output is removed whole; one that also holds uncommitted changes or
// unpushed commits keeps them and loses only its build output, as does an
// unfinished one left untouched for the grace. A live session's worktree
// and a locked one are never touched. A worktree whose gitfile names a
// repository that no longer exists is an orphan (Dangling).
package worktrees

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
)

// Grace is how long a pull request must be closed, and a worktree's HEAD
// and index untouched, before the worktree counts as finished: its worker
// may still prove the merge from it.
const Grace = 6 * time.Hour

// DetachedIdle is how long a worktree on a detached HEAD (a release's or a
// pull request's proof checkout) must be untouched before it counts as
// finished: no pull request says when its work ended.
const DetachedIdle = 72 * time.Hour

// maxDepth bounds how deep under a root a worktree is looked for:
// <root>/<repo>/<branch>, a branch with slashes a level or two more.
const maxDepth = 5

// Regenerable are the directories a build, an install or a test run writes
// and the next one writes again, by name or, with a slash, by their last
// path elements: untracked or ignored, one of these is build output
// wherever it lies.
var Regenerable = []string{
	"node_modules", "dist", "dist-types", "bin", "build", "out", "target",
	".turbo", ".next", ".cache", ".yarn/cache", "coverage",
	"__pycache__", ".pytest_cache", ".venv",
}

// Git runs git in dir and returns its standard output.
type Git func(ctx context.Context, dir string, args ...string) ([]byte, error)

// Tree is one linked worktree as git reports it.
type Tree struct {
	Dir string
	// Repo is the GitHub repository of its origin remote (owner/repo), ""
	// for none; Branch the branch checked out, "" when HEAD is detached;
	// Pushed the branch on origin it tracks, "" for none.
	Repo, Branch, Pushed string
	Head                 string
	// Locked says git worktree lock protects it.
	Locked bool
	// Work are the uncommitted changes that are real work: tracked
	// changes and untracked paths that are no build output.
	Work []string
	// Build are the untracked or ignored paths (git status's units,
	// directories end in /) that are build output: Regenerable ones.
	Build []string
	// Unpushed are the commits on HEAD no remote-tracking branch has.
	Unpushed int
	// Touched is when its HEAD, reflog or index last changed.
	Touched time.Time
}

// Find lists the linked worktrees under roots, sorted; a root that does not
// exist has none. It does not look inside a worktree for more.
func Find(roots []string) []string {
	var out []string
	for _, root := range roots {
		root = filepath.Clean(root)
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			if linked(p) {
				out = append(out, p)
				return filepath.SkipDir
			}
			if p != root && (strings.HasPrefix(d.Name(), ".") || strings.Count(p[len(root):], string(filepath.Separator)) >= maxDepth) {
				return filepath.SkipDir
			}
			return nil
		})
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// linked reports whether dir is a linked worktree: its .git is a file
// naming a gitdir under its repository's worktrees.
func linked(dir string) bool {
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(dir, ".git")))
	if err != nil {
		return false
	}
	g, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir: ")
	return ok && strings.Contains(filepath.ToSlash(g), "/worktrees/")
}

// Dangling reports whether dir is a linked worktree whose gitfile names a
// gitdir that no longer exists: its repository went, or pruned it, and git
// cannot read it.
func Dangling(dir string) bool {
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(dir, ".git")))
	if err != nil {
		return false
	}
	g, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir: ")
	if !ok {
		return false
	}
	if !filepath.IsAbs(g) {
		g = filepath.Join(dir, g)
	}
	_, err = os.Stat(filepath.Clean(g)) //nolint:gosec // only stats the gitdir a worktree under the configured roots names
	return errors.Is(err, fs.ErrNotExist)
}

// Inspect reads dir's state through git. git status runs without its
// optional index refresh, so the look changes nothing Touched reads.
func Inspect(ctx context.Context, git Git, dir string) (Tree, error) {
	return InspectBuildDirs(ctx, git, dir, nil)
}

// InspectBuildDirs includes additional explicitly configured build directories.
func InspectBuildDirs(ctx context.Context, git Git, dir string, extra []string) (Tree, error) {
	t := Tree{Dir: dir}
	gitdir, err := git(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return t, err
	}
	gd := strings.TrimSpace(string(gitdir))
	for _, f := range []string{"HEAD", "index", "logs/HEAD"} {
		if fi, err := os.Stat(filepath.Join(gd, f)); err == nil && fi.ModTime().After(t.Touched) {
			t.Touched = fi.ModTime()
		}
	}
	_, err = os.Stat(filepath.Join(gd, "locked"))
	t.Locked = err == nil
	if out, err := git(ctx, dir, "remote", "get-url", "origin"); err == nil {
		t.Repo = RepoOf(strings.TrimSpace(string(out)))
	}
	head, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return t, err
	}
	t.Head = strings.TrimSpace(string(head))
	if out, err := git(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		t.Branch = strings.TrimSpace(string(out))
	}
	if out, err := git(ctx, dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); err == nil {
		if b, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "origin/"); ok {
			t.Pushed = b
		}
	}
	status, err := git(ctx, dir, "status", "--porcelain=v1", "-z", "--untracked-files=normal", "--ignored=matching")
	if err != nil {
		return t, err
	}
	t.Work, t.Build = classifyBuildDirs(status, extra)
	count, err := git(ctx, dir, "rev-list", "--count", "HEAD", "--not", "--remotes")
	if err != nil {
		return t, err
	}
	if t.Unpushed, err = strconv.Atoi(strings.TrimSpace(string(count))); err != nil {
		return t, fmt.Errorf("git rev-list --count: %w", err)
	}
	return t, nil
}

// classify splits git status --porcelain=v1 -z --ignored output into the
// real work (tracked changes, untracked paths that are no build output) and
// the build output (untracked or ignored Regenerable paths). An ignored path
// that is not Regenerable is neither: it goes with a removed worktree, and
// stays in a kept one.
func classify(status []byte) (work, build []string) { return classifyBuildDirs(status, nil) }

func classifyBuildDirs(status []byte, extra []string) (work, build []string) {
	entries := bytes.Split(status, []byte{0})
	for i := 0; i < len(entries); i++ {
		e := string(entries[i])
		if len(e) < 4 {
			continue
		}
		code, path := e[:2], e[3:]
		switch code {
		case "!!", "??":
			if regenerableWith(path, extra) {
				build = append(build, path)
			} else if code == "??" {
				work = append(work, path)
			}
		default:
			work = append(work, path)
			// A rename or copy names its source in the next entry.
			if code[0] == 'R' || code[0] == 'C' {
				i++
			}
		}
	}
	return work, build
}

// HeadBranch is the branch a pull request of t names as its head: the
// branch on origin it tracks, unless that is the default branch it was cut
// from, else the branch checked out.
func (t Tree) HeadBranch() string {
	if t.Pushed != "" && !slices.Contains([]string{"main", "master"}, t.Pushed) {
		return t.Pushed
	}
	return t.Branch
}

// regenerable reports whether path lies in, or is, a Regenerable directory.
func regenerable(path string) bool { return regenerableWith(path, nil) }

func regenerableWith(path string, extra []string) bool {
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	for i := range parts {
		for _, r := range append(slices.Clone(Regenerable), extra...) {
			name := strings.Split(r, "/")
			if i+1 >= len(name) && slices.Equal(parts[i+1-len(name):i+1], name) {
				return true
			}
		}
	}
	return false
}

// RepoOf is the owner/repo of a GitHub remote URL, "" for another host.
func RepoOf(url string) string {
	for _, prefix := range []string{"https://github.com/", "git@github.com:", "ssh://git@github.com/"} {
		if rest, ok := strings.CutPrefix(url, prefix); ok {
			rest = strings.TrimSuffix(strings.TrimSuffix(rest, "/"), ".git")
			if strings.Count(rest, "/") == 1 {
				return rest
			}
		}
	}
	return ""
}

// The actions on a worktree.
const (
	Keep   = "keep"
	Remove = "remove"
	// Clean removes only the build output of a finished worktree whose
	// real work stays.
	Clean = "clean"
)

// The kinds of kept worktrees.
const (
	KeptLive     = "live"
	KeptLocked   = "locked"
	KeptNoOrigin = "no GitHub origin"
	KeptDetached = "detached HEAD"
	KeptNoPull   = "no pull request"
	KeptOpen     = "pull request open"
	KeptClosed   = "closed within the grace"
	KeptTouched  = "touched within the grace"
	KeptWork     = "holding only work"
)

// Verdict is what happens to a worktree, and why; Kept is the kind of a
// kept one.
type Verdict struct {
	Action string
	Why    string
	Kept   string
}

// Decide judges t: live says a session or a process works in it, pull is
// the newest pull request its branch opened (nil for none), pushed says its
// unpushed commits are the pull request's own (its head, or before it).
//
// A worktree kept for any other reason than a live session or a lock, and
// untouched Grace, still loses its build output (Clean, Kept naming why it
// stays): the next build writes it again, and git state, branch and commits
// stay.
func Decide(t Tree, pull *github.HeadPull, live, pushed bool, now time.Time) Verdict {
	switch {
	case live:
		return Verdict{Keep, "a live session or process works in it", KeptLive}
	case t.Locked:
		return Verdict{Keep, "locked", KeptLocked}
	}
	v := decide(t, pull, pushed, now)
	if v.Action == Keep && v.Kept != KeptWork && len(t.Build) > 0 && now.Sub(t.Touched) >= Grace {
		return Verdict{Action: Clean, Why: fmt.Sprintf("%s, untouched for %s", v.Why, hours(Grace)), Kept: v.Kept}
	}
	return v
}

// decide judges a worktree no live session works in and no lock holds.
func decide(t Tree, pull *github.HeadPull, pushed bool, now time.Time) Verdict {
	switch {
	case t.Repo == "":
		return Verdict{Keep, "no GitHub origin", KeptNoOrigin}
	case t.Branch == "":
		return detached(t, now)
	case pull == nil:
		return Verdict{Keep, "no pull request", KeptNoPull}
	case pull.State == github.Open:
		return Verdict{Keep, fmt.Sprintf("pull request #%d open", pull.N), KeptOpen}
	case now.Sub(pull.ClosedAt) < Grace:
		return Verdict{Keep, fmt.Sprintf("pull request #%d closed within %s", pull.N, hours(Grace)), KeptClosed}
	case now.Sub(t.Touched) < Grace:
		return Verdict{Keep, fmt.Sprintf("touched within %s", hours(Grace)), KeptTouched}
	}
	return finished(t, fmt.Sprintf("pull request #%d %s", pull.N, strings.ToLower(pull.State)), pushed)
}

// detached judges a worktree on a detached HEAD: finished once it stayed
// untouched DetachedIdle, then removed, or, while it holds work, stripped of
// its build output.
func detached(t Tree, now time.Time) Verdict {
	if now.Sub(t.Touched) < DetachedIdle {
		return Verdict{Keep, fmt.Sprintf("detached HEAD touched within %s", hours(DetachedIdle)), KeptDetached}
	}
	return finished(t, fmt.Sprintf("detached HEAD untouched for %s", hours(DetachedIdle)), false)
}

// finished judges a finished worktree, done saying why it is: removed when
// it holds nothing but build output, stripped of that while it also holds
// uncommitted changes or commits pushed says are not the pull request's,
// kept whole while it holds only those.
func finished(t Tree, done string, pushed bool) Verdict {
	var held []string
	if len(t.Work) > 0 {
		held = append(held, fmt.Sprintf("%d uncommitted (%s)", len(t.Work), strings.Join(t.Work[:min(len(t.Work), 3)], ", ")))
	}
	if t.Unpushed > 0 && !pushed {
		held = append(held, fmt.Sprintf("%d unpushed commits", t.Unpushed))
	}
	if len(held) == 0 {
		return Verdict{Action: Remove, Why: done}
	}
	why := done + ", holds " + strings.Join(held, " and ")
	if len(t.Build) == 0 {
		return Verdict{Keep, why, KeptWork}
	}
	return Verdict{Action: Clean, Why: why}
}

// hours says d as whole hours, "72h".
func hours(d time.Duration) string { return fmt.Sprintf("%dh", int(d.Hours())) }

// Writable gives the owner write and search permission on every directory
// under path, path included, as go clean -modcache does: a read-only one (a
// Go module cache copy, envtest's binaries under go/bin/k8s) refuses the
// unlink of what it holds, while a read-only file in a writable directory
// unlinks. Symbolic links are not followed, path itself included, and no
// change reaches outside path.
func Writable(path string) error {
	if fi, err := os.Lstat(path); err != nil || !fi.IsDir() {
		return nil
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	var errs []error
	_ = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			errs = append(errs, err)
			return fs.SkipDir
		}
		if fi.Mode().Perm()&0o700 != 0o700 {
			if err := root.Chmod(p, fi.Mode().Perm()|0o700); err != nil {
				errs = append(errs, err)
				return fs.SkipDir
			}
		}
		return nil
	})
	return errors.Join(errs...)
}

// RemoveAll removes path and everything under it, read-only directories
// included (Writable).
func RemoveAll(path string) error {
	if err := Writable(path); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

// Size is the disk space in bytes the files under path take, as du counts
// it (the apparent size where the platform says no blocks).
func Size(path string) int64 {
	var n int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			n += allocated(fi)
		}
		return nil
	})
	return n
}

// Allocated is the space one file takes on disk.
func Allocated(fi fs.FileInfo) int64 { return allocated(fi) }
