// Package gocache keeps the Go build cache of the machine's user, which
// every session's builds share, under a size: the entries used least
// recently go first, as in Go's own trim, which drops only those unused for
// five days.
package gocache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// ErrBusy stops a trim: a Go build began while it ran.
var ErrBusy = errors.New("a go build runs")

// lockName is the cache's use lock, a file at its top, where the go command
// keeps only its README and trim.txt: a beekeeper run holds it shared for
// the length of its command (Use), a trim exclusive for the length of the
// trim (TryTrim) and a clear for the length of the clear (Hold), so no trim
// or clear removes an entry a running compile reads.
const lockName = "beekeeper-use.lock"

// turnstileName is the lock a clear holds exclusive while it waits for the
// runs that hold the use lock, and Use passes through shared before it
// takes it: a run that starts while a clear waits waits behind it, so
// overlapping runs never keep a clear waiting for ever.
const turnstileName = "beekeeper-clear.lock"

// HeldEnv names, in a beekeeper run's environment, the cache whose use lock
// the run holds: a clear started inside the run would wait on its own run.
const HeldEnv = "BEEKEEPER_GOCACHE_HELD"

// lockRetry is how often Use asks again for a lock a trim holds.
const lockRetry = 250 * time.Millisecond

func useLock(dir string) (*flock.Flock, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return flock.New(filepath.Join(dir, lockName)), nil
}

// Use takes the cache's use lock shared for a command that may compile:
// it waits for a trim or a clear under way or waiting, until ctx ends. Many
// commands hold it at once; release gives it back.
func Use(ctx context.Context, dir string) (release func(), err error) {
	l, err := useLock(dir)
	if err != nil {
		return nil, err
	}
	turn := flock.New(filepath.Join(dir, turnstileName))
	ok, err := turn.TryRLockContext(ctx, lockRetry)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ctx.Err()
	}
	defer func() { _ = turn.Unlock() }()
	ok, err = l.TryRLockContext(ctx, lockRetry)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ctx.Err()
	}
	return func() { _ = l.Unlock() }, nil
}

// TryTrim takes the cache's use lock exclusive for a trim, without waiting:
// ok false while a command holds it (Use).
func TryTrim(dir string) (release func(), ok bool, err error) {
	l, err := useLock(dir)
	if err != nil {
		return nil, false, err
	}
	if ok, err = l.TryLock(); err != nil || !ok {
		return nil, false, err
	}
	return func() { _ = l.Unlock() }, true, nil
}

// Hold takes the cache's use lock exclusive for a clear: it waits until no
// command holds it (Use) and no trim runs, until ctx ends, and calls
// waiting once when it has to wait. Runs that start meanwhile wait behind
// it. release gives both locks back.
func Hold(ctx context.Context, dir string, waiting func()) (release func(), err error) {
	l, err := useLock(dir)
	if err != nil {
		return nil, err
	}
	turn := flock.New(filepath.Join(dir, turnstileName))
	ok, err := turn.TryLockContext(ctx, lockRetry)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ctx.Err()
	}
	if ok, err = l.TryLock(); err == nil && !ok {
		waiting()
		ok, err = l.TryLockContext(ctx, lockRetry)
	}
	if err != nil || !ok {
		_ = turn.Unlock()
		if err == nil {
			err = ctx.Err()
		}
		return nil, err
	}
	return func() { _ = l.Unlock(); _ = turn.Unlock() }, nil
}

// ModDir is the module cache the go command uses: $GOMODCACHE, else pkg/mod
// in the first $GOPATH entry, else in go under the home directory; "" when
// unknown.
func ModDir() string {
	if d := os.Getenv("GOMODCACHE"); d != "" {
		return d
	}
	if p := filepath.SplitList(os.Getenv("GOPATH")); len(p) > 0 && p[0] != "" {
		return filepath.Join(p[0], "pkg", "mod")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "go", "pkg", "mod")
}

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

// Entry is one file of the cache: an action, an output or an executable.
type Entry struct {
	Path string
	Size int64
	// Used is its modification time, which the go command moves on when it
	// uses an entry last touched over an hour before. For an executable it
	// is the enclosing output directory's modification time.
	Used time.Time
}

// Usage is what a scan found: the entries, least recently used first, and
// their total size.
type Usage struct {
	Dir     string
	Entries []Entry
	Bytes   int64

	// rootInfo identifies the directory actually scanned, even if Dir is replaced.
	rootInfo fs.FileInfo
}

// Scan reads the cache's entries: the files of its 256 two-hex-digit
// subdirectories, including executable files inside hash-d directories.
// Everything else (its README, trim.txt, fuzz corpora) is left out, and a
// missing cache is empty.
func Scan(dir string) (Usage, error) {
	u := Usage{Dir: dir}
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return u, nil
	}
	if err != nil {
		return u, err
	}
	defer func() { _ = root.Close() }()
	u.rootInfo, err = root.Stat(".")
	if err != nil {
		return u, err
	}
	subs, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return u, err
	}
	for _, s := range subs {
		if !s.IsDir() || !hexByte(s.Name()) {
			continue
		}
		files, err := fs.ReadDir(root.FS(), s.Name())
		if err != nil {
			return u, err
		}
		for _, f := range files {
			if f.IsDir() && executableDir(f.Name()) {
				info, err := f.Info()
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					return u, err
				}
				if err := scanExecutable(&u, filepath.Join(dir, s.Name(), f.Name()), info.ModTime()); err != nil {
					return u, err
				}
				continue
			}
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

// Go stores an executable in a directory named for its output hash. Its
// cache hits update that directory's mtime rather than the binary's.
func executableDir(name string) bool {
	return len(name) == 66 && strings.HasSuffix(name, "-d") && strings.Trim(name[:64], "0123456789abcdef") == ""
}

func scanExecutable(u *Usage, dir string, used time.Time) error {
	files, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, f := range files {
		if !f.Type().IsRegular() {
			continue
		}
		info, err := f.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		u.Entries = append(u.Entries, Entry{Path: filepath.Join(dir, f.Name()), Size: info.Size(), Used: used})
		u.Bytes += info.Size()
	}
	return nil
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
// command takes a missing entry for a miss and builds it again. Removals
// are confined to the opened root even when paths change concurrently. For
// Usage returned by Scan, the root must still identify the scanned directory.
func Trim(u Usage, target int64, busy func() bool) (freed int64, removed int, err error) {
	if u.Bytes <= target || len(u.Entries) == 0 {
		return 0, 0, nil
	}
	root, err := os.OpenRoot(u.Dir)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Stat(".")
	if err != nil {
		return 0, 0, err
	}
	if u.rootInfo != nil && !os.SameFile(u.rootInfo, info) {
		return 0, 0, errors.New("cache root changed since scan")
	}
	left := u.Bytes
	for i, e := range u.Entries {
		if left <= target {
			break
		}
		if i%busyEvery == 0 && busy() {
			return freed, removed, ErrBusy
		}
		// Detect replacement since opening as well as between Scan and Trim.
		// The open root confines Remove even if paths change after this check.
		current, err := os.Stat(u.Dir)
		if err != nil {
			return freed, removed, err
		}
		if !os.SameFile(info, current) {
			return freed, removed, errors.New("cache root changed since scan")
		}
		name, err := filepath.Rel(u.Dir, e.Path)
		if err != nil {
			return freed, removed, err
		}
		if !filepath.IsLocal(name) || name == "." {
			return freed, removed, fmt.Errorf("invalid cache entry %q", name)
		}
		left -= e.Size
		if err := root.Remove(name); err != nil {
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
