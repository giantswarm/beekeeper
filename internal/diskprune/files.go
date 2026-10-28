// Package diskprune reclaims explicitly disposable disk data, keeping
// process references, recent data, symlinks and checkouts intact.
package diskprune

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/worktrees"
)

const checkoutMetadata = ".git"

// InUse reports whether a candidate contains a live reference, including
// references reached through a symlink. A reference to a parent alone does
// not make all its children busy.
func InUse(dir string, refs []string) bool {
	dir = canonical(dir)
	for _, p := range refs {
		p = canonical(p)
		if p == dir || strings.HasPrefix(p, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func canonical(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
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
			return 0, "contains a checkout", nil
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
			why = "contains a checkout"
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
	name := filepath.Base(dir)
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
	return root.RemoveAll(filepath.Base(dir))
}

// Result is one cleanup's measured bytes and keep/error reasons.
type Result struct {
	Freed   int64
	Removed int
	Kept    map[string]int
	Errors  []error
}

// Add records another cleanup in the same report.
func (r *Result) Add(other Result) {
	r.Freed += other.Freed
	r.Removed += other.Removed
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
	r := Result{}
	if InUse(dir, refs) {
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
	for _, e := range entries {
		r.Add(Output(filepath.Join(base, e.Name()), cutoff, refs, dry))
	}
	return r
}
