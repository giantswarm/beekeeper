package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/gocache"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// The Go build cache every session's builds share grows without bound on a
// busy machine: Go trims only the entries unused for five days. Over
// doctor.goCacheMaxGiB the watch trims it, least recently used first, down
// to three quarters of the cap, never while a go build runs, and logs each
// trim (gocache.trim). One process trims at a time (gocache.lock), and the
// watches share when the cache was read last (gocache.json).

const (
	goCacheFile = "gocache.json"
	goCacheLock = "gocache.lock"
	// goCacheRetry is how soon a watch looks again at a trim a build held
	// back.
	goCacheRetry = 5 * time.Minute
	goCacheVerb  = "gocache.trim"
)

// goCacheMark is the watches' shared memory of the cache: when its size
// was read last, and since when a trim waits for the builds.
type goCacheMark struct {
	Checked time.Time `json:"checked"`
	Waiting time.Time `json:"waiting,omitzero"`
}

// goCacheRun is one look at the cache.
type goCacheRun struct {
	dir           string
	max           int64
	before, after int64
	removed       int
	// waits are the go builds a trim waits for; locked says another
	// process trims the cache now.
	waits  []int
	locked bool
	err    error
}

// over says the cache is over its cap.
func (r goCacheRun) over() bool { return r.before > r.max }

// String says the run in one line.
func (r goCacheRun) String() string {
	head := fmt.Sprintf("Go build cache %s: %s, cap %s (doctor.goCacheMaxGiB)", r.dir, gibStr(r.before), gibStr(r.max))
	switch {
	case r.max == 0:
		return "Go build cache: no cap (doctor.goCacheMaxGiB negative or GOCACHE off)"
	case r.locked:
		return fmt.Sprintf("Go build cache %s: another beekeeper trims it now", r.dir)
	case r.err != nil && r.removed > 0:
		return fmt.Sprintf("%s; trimmed to %s, %d entries removed, then: %v", head, gibStr(r.after), r.removed, r.err)
	case r.err != nil:
		return fmt.Sprintf("%s; %v", head, r.err)
	case len(r.waits) > 0 && r.removed > 0:
		return fmt.Sprintf("%s; trimmed to %s, %d entries removed, the rest waits for the go builds (PIDs %s)", head, gibStr(r.after), r.removed, pidList(r.waits))
	case len(r.waits) > 0:
		return fmt.Sprintf("%s; its trim waits for the go builds (PIDs %s)", head, pidList(r.waits))
	case r.removed > 0:
		return fmt.Sprintf("%s; trimmed to %s, %d entries removed least recently used first", head, gibStr(r.after), r.removed)
	case r.over():
		return head + "; would trim it to " + gibStr(r.max*3/4)
	}
	return head
}

func pidList(pids []int) string {
	s := make([]string, len(pids))
	for i, p := range pids {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ", ")
}

// notable says the run is worth a doctor's line: the cache over its cap,
// trimmed, held by another trim or unreadable.
func (r goCacheRun) notable() bool {
	return r.max > 0 && (r.over() || r.removed > 0 || r.locked || r.err != nil)
}

// goBuilds are the PIDs of the caller's running go commands.
func goBuilds() []int { return proc.Named("go", os.Getuid()) }

// trimGoCache reads the size of the Go build cache and, unless dryRun,
// trims it under the lock when it is over the cap and no go build runs
// (builds), logging what it freed.
func (a *app) trimGoCache(dryRun bool, builds func() []int) goCacheRun {
	r := goCacheRun{dir: gocache.Dir(), max: a.cfg.Doctor.GoCacheMax()}
	if r.dir == "" {
		r.max = 0
	}
	if r.max == 0 {
		return r
	}
	lock := flock.New(filepath.Join(a.store.Dir(), goCacheLock))
	ok, err := lock.TryLock()
	if err != nil || !ok {
		r.locked, r.err = err == nil, err
		return r
	}
	defer func() { _ = lock.Unlock() }()
	u, err := gocache.Scan(r.dir)
	r.before, r.after, r.err = u.Bytes, u.Bytes, err
	if err != nil || !r.over() || dryRun {
		return r
	}
	if r.waits = builds(); len(r.waits) == 0 {
		var freed int64
		freed, r.removed, err = gocache.Trim(u, r.max*3/4, func() bool { r.waits = builds(); return len(r.waits) > 0 })
		r.after -= freed
		if !errors.Is(err, gocache.ErrBusy) {
			r.err = err
		}
	}
	if r.removed > 0 {
		_ = a.store.Log(event(watchParty, goCacheVerb, "%s", r))
	}
	return r
}

// goCache reads the size of the Go build cache every doctor.goCacheEvery
// and trims it over its cap, in the background so the sample goes on. A
// trim the builds hold back is tried again every goCacheRetry; once it has
// waited doctor.goCacheEvery the watch says GO CACHE until it ran. A failed
// read or trim is GO CACHE too. The goCaching flag orders the looks: the
// next look's time is read only with the flag held, after the look that
// wrote it gave the flag back.
func (w *watcher) goCache(now time.Time) {
	if !w.goCacheOn || w.cfg.Doctor.GoCacheMax() == 0 || !w.goCaching.CompareAndSwap(false, true) {
		return
	}
	if now.Before(w.goCacheNext) {
		w.goCaching.Store(false)
		return
	}
	go func() {
		defer w.goCaching.Store(false)
		every := w.cfg.Doctor.GoCacheEvery.Duration
		var m goCacheMark
		_, _ = w.store.ReadFile(goCacheFile, &m)
		if m.Waiting.IsZero() && now.Sub(m.Checked) < every {
			w.goCacheNext = m.Checked.Add(every)
			return
		}
		r := w.trimGoCache(false, goBuilds)
		switch {
		case r.locked:
			w.goCacheNext = now.Add(goCacheRetry)
			return
		case len(r.waits) > 0:
			if m.Waiting.IsZero() {
				m.Waiting = now
			}
			w.goCacheNext = now.Add(goCacheRetry)
		default:
			m.Waiting = time.Time{}
			w.goCacheNext = now.Add(every)
		}
		m.Checked = now
		_ = w.store.WriteFile(goCacheFile, m)
		waited := !m.Waiting.IsZero() && now.Sub(m.Waiting) >= every
		w.check("gocache", r.err != nil || waited, "GO CACHE: %s", r)
	}()
}
