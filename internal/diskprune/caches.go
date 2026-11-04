package diskprune

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// CacheMinAge protects what a tool may still be writing: no cache unit
// written within it is removed, whatever its policy says.
const CacheMinAge = time.Hour

// Policy is one cache root's rule. In entries mode the root's direct
// children are the units, each removed whole; in files mode its regular
// files. A unit untouched for OlderThan goes; past MaxBytes the least
// recently written go until the root fits; under pressure a Regenerable
// cache drops every unit older than CacheMinAge.
type Policy struct {
	Root        string
	Files       bool
	OlderThan   time.Duration
	MaxBytes    int64
	Regenerable bool
	// Nested are other policies' roots inside this one: each governs its
	// own subtree, which this policy neither counts nor removes.
	Nested []string
	// Recheck reads the processes' references again once the scan is done
	// and before anything is removed; an error keeps the whole root. Nil
	// keeps the references the call was given.
	Recheck func() ([]string, error)
}

// CacheResult is one root's outcome: Held is what it held before, Newest
// its last write.
type CacheResult struct {
	Result
	Held   int64
	Newest time.Time
}

type unit struct {
	path   string
	size   int64
	newest time.Time
	why    string
}

// Cache trims root by p. A root that is a symbolic link, holds a protected
// path (the home, the state directory, a checkout root) or is itself a
// checkout is kept whole. A unit a process references, one holding a git
// checkout (work not yet committed or pushed included) or one written within
// CacheMinAge is kept; in files mode one reference or checkout inside the
// root keeps all of it. A dry run measures without writes.
func Cache(p Policy, now time.Time, pressure bool, refs, protect []string, dry bool) CacheResult {
	var r CacheResult
	fi, err := os.Lstat(p.Root)
	if errors.Is(err, os.ErrNotExist) {
		return r
	}
	if err != nil {
		r.Errors = append(r.Errors, err)
		return r
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		r.Keep("not a directory")
		return r
	}
	for _, q := range protect {
		if q != "" && InUse(p.Root, []string{q}) {
			r.Keep("holds protected state")
			return r
		}
	}
	root, err := os.OpenRoot(p.Root)
	if err != nil {
		r.Errors = append(r.Errors, err)
		return r
	}
	defer func() { _ = root.Close() }()
	opened, err := root.Stat(".")
	if err != nil {
		r.Errors = append(r.Errors, err)
		return r
	}
	if !os.SameFile(fi, opened) {
		r.Keep(whyRootChanged)
		return r
	}
	var units []unit
	if p.Files {
		u, ok := scanFiles(root, p.Root, refs, p.Nested, &r)
		if !ok {
			return r
		}
		units = u
	} else {
		entries, err := fs.ReadDir(root.FS(), ".")
		if err != nil {
			r.Errors = append(r.Errors, err)
			return r
		}
		for _, e := range entries {
			if e.Name() == checkoutMetadata {
				r.Keep(whyCheckout)
				return r
			}
		}
		for _, e := range entries {
			path := filepath.Join(p.Root, e.Name())
			if nestedIn(path, p.Nested) || InUse(path, p.Nested) {
				r.Keep("another policy's cache")
				continue
			}
			u := inspectFS(root.FS(), e.Name(), path)
			if u.why == "" && InUse(u.path, refs) {
				u.why = "in use"
			}
			units = append(units, u)
		}
	}
	for _, u := range units {
		r.Held += u.size
		if u.newest.After(r.Newest) {
			r.Newest = u.newest
		}
	}
	floor := now.Add(-CacheMinAge)
	var cutoff time.Time
	if p.OlderThan > 0 {
		cutoff = now.Add(-p.OlderThan)
	}
	if pressure && p.Regenerable {
		cutoff = floor
	}
	if cutoff.After(floor) {
		cutoff = floor
	}
	slices.SortFunc(units, func(a, b unit) int { return a.newest.Compare(b.newest) })
	if !dry && p.Recheck != nil {
		// The scan of a large cache takes a while: whatever a process
		// opened meanwhile is in use now.
		if refs, err = p.Recheck(); err != nil {
			r.Keep("process references unknown")
			return r
		}
		if p.Files && filesInUse(p.Root, refs, p.Nested) {
			r.Keep("in use")
			return r
		}
	}
	if !dry {
		current, err := os.Lstat(p.Root)
		if err != nil || !os.SameFile(opened, current) {
			r.Keep(whyRootChanged)
			return r
		}
	}
	left := r.Held
	emptied := map[string]bool{}
	for _, u := range units {
		if u.why != "" {
			r.Keep(u.why)
			continue
		}
		old := !cutoff.IsZero() && u.newest.Before(cutoff)
		over := p.MaxBytes > 0 && left > p.MaxBytes && u.newest.Before(floor)
		if !old && !over {
			if !u.newest.Before(floor) {
				r.Keep("written within " + CacheMinAge.String())
			}
			continue
		}
		if !dry {
			var err error
			if p.Files {
				err = removeFile(root, p.Root, u)
			} else {
				err = removeEntry(root, u, refs)
			}
			if errors.Is(err, errChanged) {
				r.Keep("written during the prune")
				continue
			}
			if errors.Is(err, errInUse) {
				r.Keep("in use")
				continue
			}
			if errors.Is(err, os.ErrNotExist) {
				left -= u.size
				continue // gone already: nothing freed by this run
			}
			if err != nil {
				r.Errors = append(r.Errors, fmt.Errorf("cache removal: %w", err))
				continue
			}
		}
		left -= u.size
		r.Freed += u.size
		r.Removed++
		emptied[filepath.Dir(u.path)] = true
	}
	if p.Files && !dry {
		removeEmptied(root, emptied)
	}
	return r
}

// nestedIn reports whether path lies inside (or is) one of roots.
func nestedIn(path string, roots []string) bool {
	for _, n := range roots {
		if InUse(n, []string{path}) {
			return true
		}
	}
	return false
}

// removeEntry removes one entry whole once it still qualifies: inspected
// again right before, it must be unreferenced, hold no checkout and have no
// write newer than the scan saw. A writer that started meanwhile keeps it.
func removeEntry(root *os.Root, u unit, refs []string) error {
	if InUse(u.path, refs) {
		return errInUse
	}
	name := filepath.Base(u.path)
	v := inspectFS(root.FS(), name, u.path)
	if v.why == whyUnreadable {
		if _, err := root.Lstat(name); errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if v.why != "" || v.newest.After(u.newest) {
		return errChanged
	}
	return remove(root, name)
}

// whyRootChanged keeps a cache whose pathname no longer names the opened root.
const whyRootChanged = "cache root changed"

// whyUnreadable keeps what could not be inspected.
const whyUnreadable = "unreadable"

var errInUse = errors.New("in use since the scan")

var errChanged = errors.New("written since the scan")

// removeFile removes one file through the cache's root, so a directory
// swapped for a symbolic link cannot redirect it, unless it was written
// since the scan.
func removeFile(root *os.Root, base string, u unit) error {
	rel, err := filepath.Rel(base, u.path)
	if err != nil {
		return err
	}
	fi, err := root.Lstat(rel)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.ModTime().After(u.newest) {
		return errChanged
	}
	return root.Remove(rel)
}

// filesInUse checks references in this policy's domain. A nested policy
// decides use of its own subtree independently.
func filesInUse(dir string, refs, nested []string) bool {
	if len(nested) == 0 {
		return InUse(dir, refs)
	}
	for _, ref := range refs {
		if !nestedIn(ref, nested) && InUse(dir, []string{ref}) {
			return true
		}
	}
	return false
}

// scanFiles lists root's regular files outside nested policies. A reference,
// checkout or unreadable subtree in this policy's domain keeps all of it.
func scanFiles(root *os.Root, dir string, refs, nested []string, r *CacheResult) ([]unit, bool) {
	// The rooted walk calls its top directory "."; keep its logical name.
	if filepath.Base(dir) == checkoutMetadata {
		r.Keep(whyCheckout)
		return nil, false
	}
	if filesInUse(dir, refs, nested) {
		r.Keep("in use")
		return nil, false
	}
	var units []unit
	why := ""
	err := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Name() == checkoutMetadata {
			why = whyCheckout
			return fs.SkipAll
		}
		if name != "." && nestedIn(filepath.Join(dir, filepath.FromSlash(name)), nested) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // removed since its directory was read
		}
		units = append(units, unit{path: filepath.Join(dir, filepath.FromSlash(name)), size: fi.Size(), newest: fi.ModTime()})
		return nil
	})
	if err != nil {
		r.Keep(whyUnreadable)
		r.Errors = append(r.Errors, fmt.Errorf("cache inspection: %w", err))
		return nil, false
	}
	if why != "" {
		r.Keep(why)
		return nil, false
	}
	return units, true
}

// inspect measures one entry without following symbolic links: its size,
// its newest write, and why it must stay (a symbolic link, a checkout, an
// unreadable subtree), if it must.
func inspect(path string) unit {
	if path == "" {
		return unit{path: path, why: whyUnreadable}
	}
	dir, name := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	if name == "" {
		name = "."
	}
	return inspectFS(os.DirFS(dir), name, path)
}

// inspectFS uses a relative name for filesystem access, retaining path for
// reference checks and reports. An opened root keeps it stable across renames.
func inspectFS(fsys fs.FS, name, path string) unit {
	u := unit{path: path}
	name = filepath.ToSlash(name)
	fi, err := fs.Lstat(fsys, name)
	if err != nil {
		u.why = whyUnreadable
		return u
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		u.why = "symbolic link"
		return u
	}
	u.newest = fi.ModTime()
	if !fi.IsDir() {
		u.size = fi.Size()
		if !fi.Mode().IsRegular() {
			u.why = "not disposable output"
		}
		return u
	}
	err = fs.WalkDir(fsys, name, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == checkoutMetadata {
			u.why = whyCheckout
			if d.IsDir() {
				return fs.SkipDir
			}
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.ModTime().After(u.newest) {
			u.newest = fi.ModTime()
		}
		if fi.Mode().IsRegular() {
			u.size += fi.Size()
		}
		return nil
	})
	if err != nil && u.why == "" {
		u.why = whyUnreadable
	}
	return u
}

// Measure is a directory's size and newest write, for reports.
func Measure(path string) (int64, time.Time) {
	u := inspect(path)
	return u.size, u.newest
}

// removeEmptied removes the directories a trim left empty, and their
// parents it left empty in turn, deepest first; root itself stays.
func removeEmptied(root *os.Root, dirs map[string]bool) {
	base := root.Name()
	todo := make([]string, 0, len(dirs))
	for d := range dirs {
		todo = append(todo, d)
	}
	slices.SortFunc(todo, func(a, b string) int {
		return strings.Count(b, string(filepath.Separator)) - strings.Count(a, string(filepath.Separator))
	})
	for _, d := range todo {
		for d != base && strings.HasPrefix(d, base+string(filepath.Separator)) {
			name, err := filepath.Rel(base, d)
			if err != nil || root.Remove(name) != nil { // fails on a directory still holding entries
				break
			}
			d = filepath.Dir(d)
		}
	}
}
