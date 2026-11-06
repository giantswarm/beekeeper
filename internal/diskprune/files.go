// Package diskprune reclaims explicitly disposable disk data, keeping
// process references, recent data, symlinks and checkouts intact.
package diskprune

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/giantswarm/beekeeper/internal/worktrees"
)

const checkoutMetadata = ".git"

// evalSymlinks resolves a path; tests count its calls.
var evalSymlinks = filepath.EvalSymlinks

// whyCheckout keeps data holding a git checkout.
const whyCheckout = "contains a checkout"

// InUse reports whether a candidate contains a live reference, including
// references reached through a symlink. A reference to a parent alone does
// not make all its children busy.
func InUse(dir string, refs []string) bool { return NewRefs(refs).Holds(dir) }

// Refs is live references resolved once. A scan asks about every candidate:
// resolving each reference again per candidate costs candidates x references
// x path depth syscalls, which kept a dry run going for minutes.
type Refs struct{ sorted []string }

// NewRefs resolves refs as they are now; a later read needs a new Refs.
func NewRefs(refs []string) Refs {
	sorted := make([]string, 0, len(refs))
	for _, p := range refs {
		sorted = append(sorted, canonical(p))
	}
	slices.Sort(sorted)
	return Refs{slices.Compact(sorted)}
}

// Holds is InUse against the resolved references.
func (r Refs) Holds(dir string) bool {
	dir = canonical(dir)
	if _, found := slices.BinarySearch(r.sorted, dir); found {
		return true
	}
	inside := dir + string(filepath.Separator)
	i, _ := slices.BinarySearch(r.sorted, inside)
	return i < len(r.sorted) && strings.HasPrefix(r.sorted[i], inside)
}

func canonical(p string) string {
	if r, err := evalSymlinks(p); err == nil {
		return r
	}
	p = filepath.Clean(p)
	parent := filepath.Dir(p)
	if parent != p {
		return filepath.Join(canonical(parent), filepath.Base(p))
	}
	return p
}

// Disposable inspects every entry without following symlinks. An unknown
// directory, checkout, unreadable subtree or recent file is kept.
func Disposable(dir string, cutoff time.Time) (int64, string, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return 0, "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return 0, "symbolic link", nil
	}
	if fi.Mode().IsRegular() {
		if fi.Name() == checkoutMetadata {
			return 0, whyCheckout, nil
		}
		if !cutoff.IsZero() && fi.ModTime().After(cutoff) {
			return 0, "recently used", nil
		}
		return worktrees.Size(dir), "", nil
	}
	if !fi.IsDir() {
		return 0, "not disposable output", nil
	}
	var why string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == checkoutMetadata {
			why = whyCheckout
			return fs.SkipAll
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if !cutoff.IsZero() && fi.ModTime().After(cutoff) {
			why = "recently used"
			return fs.SkipAll
		}
		return nil
	})
	if err != nil || why != "" {
		return 0, why, err
	}
	return worktrees.Size(dir), "", nil
}

// Remove deletes disposable output through its parent root: an intervening
// symlink cannot redirect removal outside the candidate's parent.
func Remove(dir string) error {
	root, err := os.OpenRoot(filepath.Dir(dir))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return remove(root, filepath.Base(dir))
}

// remove keeps inspection and deletion beneath the already opened parent.
func remove(root *os.Root, name string) error {
	fi, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		child, err := root.OpenRoot(name)
		if err != nil {
			return err
		}
		defer func() { _ = child.Close() }()
		err = fs.WalkDir(child.FS(), ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Name() == checkoutMetadata {
				return errors.New("refused: output contains a checkout")
			}
			if d.IsDir() {
				fi, err := d.Info()
				if err != nil {
					return err
				}
				return child.Chmod(p, fi.Mode().Perm()|0700)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return root.RemoveAll(name)
}

// Result is one cleanup's measured bytes and keep/error reasons.
type Result struct {
	Freed   int64
	Removed int
	Moved   int64
	Staged  int
	Kept    map[string]int
	Errors  []error
}

// Add records another cleanup in the same report.
func (r *Result) Add(other Result) {
	r.Freed += other.Freed
	r.Removed += other.Removed
	r.Moved += other.Moved
	r.Staged += other.Staged
	if r.Kept == nil {
		r.Kept = map[string]int{}
	}
	for k, n := range other.Kept {
		r.Kept[k] += n
	}
	r.Errors = append(r.Errors, other.Errors...)
}

// Keep records a reason without naming private paths in the watch line.
func (r *Result) Keep(why string) {
	if r.Kept == nil {
		r.Kept = map[string]int{}
	}
	r.Kept[why]++
}

// Output reclaims a known output directory, refusing checkouts, active
// references and data newer than cutoff. A dry run measures without writes.
func Output(dir string, cutoff time.Time, refs []string, dry bool) Result {
	return output(dir, cutoff, NewRefs(refs), dry)
}

func output(dir string, cutoff time.Time, refs Refs, dry bool) Result {
	r := Result{}
	if refs.Holds(dir) {
		r.Keep("in use")
		return r
	}
	n, why, err := Disposable(dir, cutoff)
	if errors.Is(err, os.ErrNotExist) {
		return r
	}
	if err != nil {
		r.Errors = append(r.Errors, fmt.Errorf("output inspection: %w", err))
		return r
	}
	if why != "" {
		r.Keep(why)
		return r
	}
	if !dry {
		if err := Remove(dir); err != nil {
			r.Errors = append(r.Errors, fmt.Errorf("output removal: %w", err))
			return r
		}
	}
	r.Freed, r.Removed = n, 1
	return r
}

// Stage moves build output into trash on the same filesystem. Moving does
// not free bytes. The new entry starts its retention window now, even when
// all the output's files are old. Cross-filesystem output is retained.
func Stage(dir, trash string, now time.Time, refs []string, dry bool) Result {
	return stage(dir, trash, now, refs, dry, os.Rename)
}

func stage(dir, trash string, now time.Time, refs []string, dry bool, rename func(string, string) error) Result {
	r := Result{}
	if InUse(dir, refs) {
		r.Keep("in use")
		return r
	}
	n, why, err := Disposable(dir, time.Time{})
	if errors.Is(err, os.ErrNotExist) {
		return r
	}
	if err != nil {
		r.Errors = append(r.Errors, err)
		return r
	}
	if why != "" {
		r.Keep(why)
		return r
	}
	if trash == "" || InUse(dir, []string{trash}) || InUse(trash, []string{dir}) {
		r.Keep("unsafe trash location")
		return r
	}
	if fi, err := os.Lstat(trash); err == nil && !fi.IsDir() {
		r.Keep("trash is not a directory")
		return r
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		r.Errors = append(r.Errors, err)
		return r
	}
	if !dry {
		if err := os.MkdirAll(trash, 0700); err != nil {
			r.Errors = append(r.Errors, err)
			return r
		}
		entry, err := os.MkdirTemp(trash, "build-output-")
		if err != nil {
			r.Errors = append(r.Errors, err)
			return r
		}
		if err := rename(dir, filepath.Join(entry, filepath.Base(dir))); err != nil {
			_ = os.Remove(entry)
			if errors.Is(err, syscall.EXDEV) {
				r.Keep("output on another filesystem")
			} else {
				r.Errors = append(r.Errors, err)
			}
			return r
		}
		if err := os.Chtimes(entry, now, now); err != nil {
			r.Errors = append(r.Errors, err)
		}
	}
	r.Moved, r.Staged = n, 1
	return r
}

// Children prunes stale direct children only, never the containing directory.
func Children(base string, cutoff time.Time, refs []string, dry bool) Result {
	r := Result{}
	fi, err := os.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		return r
	}
	if err != nil {
		r.Errors = append(r.Errors, err)
		return r
	}
	if !fi.IsDir() {
		r.Keep("not a directory or symbolic link")
		return r
	}
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return r
	}
	if err != nil {
		r.Errors = append(r.Errors, err)
		return r
	}
	live := NewRefs(refs)
	for _, e := range entries {
		r.Add(output(filepath.Join(base, e.Name()), cutoff, live, dry))
	}
	return r
}
