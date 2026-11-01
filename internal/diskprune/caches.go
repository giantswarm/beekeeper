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
	var units []unit
	if p.Files {
		u, ok := scanFiles(p.Root, refs, &r)
		if !ok {
			return r
		}
		units = slices.DeleteFunc(u, func(u unit) bool { return nestedIn(u.path, p.Nested) })
	} else {
		entries, err := os.ReadDir(p.Root)
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
			u := inspect(path)
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
		if p.Files && InUse(p.Root, refs) {
			r.Keep("in use")
			return r
		}
	}
	left := r.Held
	emptied := map[string]bool{}
	var root *os.Root
	defer func() {
		if root != nil {
			_ = root.Close()
		}
	}()
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
				if root == nil {
					if root, err = os.OpenRoot(p.Root); err != nil {
						r.Errors = append(r.Errors, err)
						return r
					}
				}
				err = removeFile(root, p.Root, u)
			} else {
				err = removeEntry(u, refs)
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
		removeEmptied(p.Root, emptied)
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
func removeEntry(u unit, refs []string) error {
	if InUse(u.path, refs) {
		return errInUse
	}
	v := inspect(u.path)
	if v.why == "unreadable" {
		if _, err := os.Lstat(u.path); errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if v.why != "" || v.newest.After(u.newest) {
		return errChanged
	}
	return Remove(u.path)
}

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

// scanFiles lists root's regular files. A reference or checkout anywhere in
// root, or an unreadable subtree, keeps all of it (ok false).
func scanFiles(dir string, refs []string, r *CacheResult) ([]unit, bool) {
	if InUse(dir, refs) {
		r.Keep("in use")
		return nil, false
	}
	var units []unit
	why := ""
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
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
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // removed since its directory was read
		}
		units = append(units, unit{path: p, size: fi.Size(), newest: fi.ModTime()})
		return nil
	})
	if err != nil {
		r.Keep("unreadable")
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
	u := unit{path: path}
	fi, err := os.Lstat(path)
	if err != nil {
		u.why = "unreadable"
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
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
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
		u.why = "unreadable"
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
func removeEmptied(root string, dirs map[string]bool) {
	todo := make([]string, 0, len(dirs))
	for d := range dirs {
		todo = append(todo, d)
	}
	slices.SortFunc(todo, func(a, b string) int {
		return strings.Count(b, string(filepath.Separator)) - strings.Count(a, string(filepath.Separator))
	})
	for _, d := range todo {
		for d != root && strings.HasPrefix(d, root+string(filepath.Separator)) {
			if os.Remove(d) != nil { // fails on a directory still holding entries
				break
			}
			d = filepath.Dir(d)
		}
	}
}
