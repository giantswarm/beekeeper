package cmd

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/free"
)

// What dead sessions and dead go commands leave in the tmpfs /tmp is RAM no
// one gives back: Claude Code keeps a session's scratch until the machine
// restarts, and a go command killed mid-build leaves its go-build dir. Every
// doctor.freeEvery the watch frees them by itself, free --apply's automatic
// sections (free.Automatic: session-dirs and go-build), and logs each run that
// freed something (free.apply). One process frees at a time (free.lock), and
// the watches share when it ran last (free.json), which the TMPFS line says.

const (
	freeFile = "free.json"
	freeLock = "free.lock"
	freeVerb = "free.apply"
)

// liveTmpfs reads the machine the automatic free acts on. Tests replace it
// (TestMain), so no test frees the machine's /tmp.
var liveTmpfs = (*app).freeTmpfsMachine

// tmpfsRun is one automatic free.
type tmpfsRun struct {
	dryRun bool
	freed  []free.Freed
	// locked says another process frees tmpfs now.
	locked bool
	err    error
}

// what lists what the run freed, or would free; "" for nothing.
func (r tmpfsRun) what() string {
	parts := make([]string, len(r.freed))
	for i, f := range r.freed {
		parts[i] = f.String()
	}
	return strings.Join(parts, "; ")
}

// String says the run in one line.
func (r tmpfsRun) String() string {
	switch {
	case r.locked:
		return "tmpfs: another beekeeper frees it now"
	case r.err != nil:
		return fmt.Sprintf("tmpfs: the automatic free failed: %v", r.err)
	case len(r.freed) == 0:
		return "tmpfs: nothing to free"
	case r.dryRun:
		return "tmpfs: would free " + r.what()
	}
	return "tmpfs: freed " + r.what()
}

// notable says the run is worth a doctor's line: it freed or would free
// something, another process frees tmpfs, or it failed.
func (r tmpfsRun) notable() bool { return len(r.freed) > 0 || r.locked || r.err != nil }

// freeTmpfs runs free's automatic sections under the lock on the machine
// read returns; with dryRun it only says what it would free.
func (a *app) freeTmpfs(dryRun bool, read func() (free.Machine, error)) tmpfsRun {
	r := tmpfsRun{dryRun: dryRun}
	lock := flock.New(filepath.Join(a.store.Dir(), freeLock))
	ok, err := lock.TryLock()
	if err != nil || !ok {
		r.locked, r.err = err == nil, err
		return r
	}
	defer func() { _ = lock.Unlock() }()
	m, err := read()
	if err != nil {
		r.err = err
		return r
	}
	if !dryRun {
		if st, err := a.store.Read(); err == nil {
			cleanupDeadRuns(a.store.Dir(), st, time.Now())
		}
	}
	o := free.Options{Apply: !dryRun, Only: free.Automatic, Stale: free.DefaultStale}
	r.freed = (&free.Run{Options: o, Machine: m, Out: io.Discard}).Free()
	if !dryRun && len(r.freed) > 0 {
		_ = a.store.Log(event(watchParty, freeVerb, "%s", r.what()))
	}
	return r
}

// watchAutoFree runs the automatic free every doctor.freeEvery, in the background
// so the sample goes on. The freeing flag orders the runs as goCaching orders
// the Go build cache's looks: freeNext is read only with the flag held.
func watchAutoFree(w *watcher, now time.Time) {
	every := w.a.cfg.Doctor.FreeEvery.Duration
	if !w.freeOn || every <= 0 || !w.freeing.CompareAndSwap(false, true) {
		return
	}
	if now.Before(w.freeNext) {
		w.freeing.Store(false)
		return
	}
	d := *w.a // reading the machine moves its now on
	w.bg.Add(1)
	go func() {
		defer w.bg.Done()
		defer w.freeing.Store(false)
		w.freeDue(func() (free.Machine, error) { return liveTmpfs(&d) }, now, every)
	}()
}

// watchFreeDue runs the automatic free on the machine read returns, unless a watch
// ran it within every. A run that freed something is one FREED line; a failed
// one is FREE FAILED until a run succeeds; a run another process holds the
// lock for is tried again after goCacheRetry.
func watchFreeDue(w *watcher, read func() (free.Machine, error), now time.Time, every time.Duration) {
	if m := w.readFreeMark(); now.Sub(m.At) < every {
		w.freeNext = m.At.Add(every)
		return
	}
	r := w.a.freeTmpfs(false, read)
	if r.locked {
		w.freeNext = now.Add(goCacheRetry)
		return
	}
	w.freeNext = now.Add(every)
	w.check("free", r.err != nil, "FREE FAILED: the automatic free of tmpfs: %v", r.err)
	if r.err != nil {
		return
	}
	m := freeMark{At: now, Freed: r.what()}
	_ = w.a.store.WriteFile(freeFile, m)
	w.lastFree.Store(&m)
	if m.Freed != "" {
		w.emitNow("free", "FREED in tmpfs: %s (doctor.freeEvery)", m.Freed)
	}
}
