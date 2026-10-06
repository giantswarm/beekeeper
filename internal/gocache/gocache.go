// Package gocache keeps the Go build cache of the machine's user, which
// every session's builds share, under a size: the entries used least
// recently go first, as in Go's own trim, which drops only those unused for
// five days.
package gocache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// ErrBusy stops a trim: a Go build began while it ran.
var ErrBusy = errors.New("a go build runs")

// busyEvery is how many removals a trim makes between asking whether a
// build began.
const busyEvery = 256

// Dir is the build cache the go command uses: $GOCACHE, else go-build in
// the user's cache directory; "" when it is off or unknown.
func Dir() string {
	if d := os.Getenv("GOCACHE"); d != "" {
		if d == "off" {
			return ""
		}
		return d
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "go-build")
}

// Entry is one file of the cache: an action or an output.
type Entry struct {
	Path string
	Size int64
	// Used is its modification time, which the go command moves on when it
	// uses an entry last touched over an hour before.
	Used time.Time
}

// Usage is what a scan found: the entries, least recently used first, and
// their total size.
type Usage struct {
	Dir     string
	Entries []Entry
	Bytes   int64
}

// Scan reads the cache's entries: the files of its 256 two-hex-digit
// subdirectories. Everything else (its README, trim.txt, fuzz corpora) is
// left out, and a missing cache is empty.
func Scan(dir string) (Usage, error) {
	u := Usage{Dir: dir}
	subs, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return u, nil
	}
	if err != nil {
		return u, err
	}
	for _, s := range subs {
		if !s.IsDir() || !hexByte(s.Name()) {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, s.Name()))
		if err != nil {
			return u, err
		}
		for _, f := range files {
			if !f.Type().IsRegular() {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue // removed since the directory was read
			}
			u.Entries = append(u.Entries, Entry{Path: filepath.Join(dir, s.Name(), f.Name()), Size: info.Size(), Used: info.ModTime()})
			u.Bytes += info.Size()
		}
	}
	slices.SortFunc(u.Entries, func(a, b Entry) int { return a.Used.Compare(b.Used) })
	return u, nil
}

func hexByte(name string) bool {
	if len(name) != 2 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Trim removes the least recently used entries until the cache holds at
// most target bytes, and returns the bytes and entries it removed. It asks
// busy before it starts and every few hundred removals, and stops with
// ErrBusy once a build runs. An entry gone already is skipped; the go
// command takes a missing entry for a miss and builds it again.
func Trim(u Usage, target int64, busy func() bool) (freed int64, removed int, err error) {
	left := u.Bytes
	for i, e := range u.Entries {
		if left <= target {
			break
		}
		if i%busyEvery == 0 && busy() {
			return freed, removed, ErrBusy
		}
		left -= e.Size
		if err := os.Remove(e.Path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return freed, removed, err
		}
		freed += e.Size
		removed++
	}
	return freed, removed, nil
}
