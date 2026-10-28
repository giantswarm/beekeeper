package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/diskprune"
	"github.com/giantswarm/beekeeper/internal/gocache"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/worktrees"
)

const diskPruneFile = "disk-prune.json"
const diskPruneLock = "disk-prune.lock"

// diskMachine is a snapshot of live references and disposable disk places.
// Tests replace the reader, so automatic pruning never reaches real data.
type diskMachine struct {
	refs           []string
	builds         bool
	pressure       bool
	cache, sccache string
	tmp, trash     []string
	trees          worktreeMachine
	refresh        func() ([]string, bool, error)
}

var liveDisk = (*app).diskMachine

func diskReferences() ([]string, bool, error) {
	refs, commands, err := proc.UserReferences(os.Getuid())
	if err != nil {
		return nil, false, err
	}
	builds := false
	for _, comm := range commands {
		if slices.Contains([]string{"go", "compile", "link", "cargo", "rustc", "cc", "gcc", "clang", "ninja", "make", "cmake"}, comm) {
			builds = true
		}
	}
	slices.Sort(refs)
	refs = slices.Compact(refs)
	return refs, builds, nil
}

func (a *app) diskMachine() (diskMachine, error) {
	m := diskMachine{refresh: diskReferences, cache: goCacheDir(), tmp: []string{filepath.Join(a.cfg.StateDir, "tmp")}, trash: a.cfg.Disk.TrashDirs}
	if d, err := machine.ReadDisk(a.cfg.StateDir); err == nil {
		m.pressure = d.FreeMiB < a.cfg.Watch.DiskMin(d.TotalMiB)
	}
	var err error
	m.refs, m.builds, err = diskReferences()
	if err != nil {
		return m, fmt.Errorf("disk pruning kept everything: process references unavailable: %w", err)
	}
	m.sccache = os.Getenv("SCCACHE_DIR")
	if m.sccache == "" {
		if cache, err := os.UserCacheDir(); err == nil {
			m.sccache = filepath.Join(cache, "sccache")
		}
	}
	m.trees, err = liveWorktrees(a)
	if err != nil {
		return m, err
	}
	roots := append(slices.Clone(a.cfg.Doctor.WorktreeRoots), filepath.Join(a.cfg.StateDir, "worktrees"))
	places := append(slices.Clone(a.cfg.Disk.Checkouts), a.cfg.Hooks.Scope.Dirs...)
	for _, r := range claude.ByCLI(a.cfg) {
		places = append(places, r.Cwd, r.OriginCwd)
	}
	// A main checkout can also be known only by its CLI transcript.
	transcripts, _ := filepath.Glob(filepath.Join(a.cfg.Claude.ProjectsDir, "*", "*.jsonl"))
	for _, p := range transcripts {
		if d, err := claude.TranscriptCwd(p); err == nil && d != "" {
			places = append(places, d)
		}
	}
	places = slices.DeleteFunc(places, func(p string) bool { return p == "" })
	m.trees.dirs = append(m.trees.dirs, worktrees.Find(roots)...)
	m.trees.dirs = append(m.trees.dirs, worktrees.FindHarness(append(places, m.trees.dirs...))...)
	for _, o := range a.cfg.Disk.BuildOutputs {
		m.trees.dirs = append(m.trees.dirs, o.Worktree)
	}
	slices.Sort(m.trees.dirs)
	m.trees.dirs = slices.Compact(m.trees.dirs)
	m.trees.roots = roots
	m.trees.automatic, m.trees.buildDirs, m.trees.outputs, m.trees.refs = true, a.cfg.Disk.BuildDirs, a.cfg.Disk.BuildOutputs, m.refs
	m.trees.live = append(m.trees.live, m.refs...)
	m.trees.recheck = func() ([]string, error) { refs, _, err := diskReferences(); return refs, err }
	return m, nil
}

type diskRun struct {
	diskprune.Result
	trees  worktreeRun
	locked bool
	dry    bool
}

func (r diskRun) String() string {
	if r.locked {
		return "disk: kept everything (another disk prune runs)"
	}
	verb := "freed"
	if r.dry {
		verb = "would free"
	}
	parts := []string{fmt.Sprintf("disk: %s %s in %d cache/output/tmp/trash entries", verb, sizeStr(r.Freed+r.trees.freed()), r.Removed)}
	if r.trees.kept != nil {
		parts = append(parts, r.trees.String())
	}
	var keeps []string
	for _, why := range sortedReasons(r.Kept) {
		keeps = append(keeps, fmt.Sprintf("%d %s", r.Kept[why], why))
	}
	if len(keeps) > 0 {
		parts = append(parts, "kept: "+strings.Join(keeps, ", "))
	}
	if err := errors.Join(r.Errors...); err != nil {
		parts = append(parts, "failed: "+err.Error())
	}
	return oneLine(strings.Join(parts, "; "))
}

// sortedReasons gives deterministic reasons without exposing individual paths.
func sortedReasons(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func (a *app) pruneDisk(dry, pressure bool, read func() (diskMachine, error)) diskRun {
	r := diskRun{dry: dry}
	lock := flock.New(filepath.Join(a.store.Dir(), diskPruneLock))
	ok, err := lock.TryLock()
	if err != nil {
		r.Errors = append(r.Errors, err)
		return r
	}
	if !ok {
		r.locked = true
		return r
	}
	defer func() { _ = lock.Unlock() }()
	m, err := read()
	if err != nil {
		r.Errors = append(r.Errors, err)
		return r
	}
	pressure = pressure || m.pressure
	a.pruneDiskCaches(&r, m, pressure)
	if m.refresh != nil {
		refs, _, err := m.refresh()
		if err != nil {
			r.Errors = append(r.Errors, err)
			return r
		}
		m.refs = refs
	}
	for _, d := range m.tmp {
		r.Add(diskprune.Children(d, a.now.Add(-time.Hour), m.refs, dry))
	}
	m.trees.automatic = true
	m.trees.refs = m.refs
	m.trees.trash = filepath.Join(a.store.Dir(), "trash")
	r.trees = a.trimWorktrees(dry, func() (worktreeMachine, error) { return m.trees, nil })
	if m.refresh != nil {
		refs, _, err := m.refresh()
		if err != nil {
			r.Errors = append(r.Errors, err)
			return r
		}
		m.refs = refs
	}
	cutoff := a.now.Add(-time.Hour)
	if pressure {
		cutoff = time.Time{}
	}
	trash := append(slices.Clone(m.trash), m.trees.trash)
	slices.Sort(trash)
	for _, d := range slices.Compact(trash) {
		r.Add(diskprune.Children(d, cutoff, m.refs, dry))
	}
	if r.trees.err != nil {
		r.Errors = append(r.Errors, r.trees.err)
	}
	if len(r.trees.failed) > 0 {
		r.Errors = append(r.Errors, fmt.Errorf("%d worktree actions failed", len(r.trees.failed)))
	}
	if !dry {
		_ = a.store.Log(event(watchParty, "disk.prune", "%s", r))
	}
	return r
}

func (a *app) pruneDiskCaches(r *diskRun, m diskMachine, pressure bool) {
	if m.builds {
		r.Keep("caches: builds running")
		return
	}
	release, ok, err := gocache.TryTrim(filepath.Join(a.store.Dir(), "build-cache"))
	if err != nil {
		r.Errors = append(r.Errors, err)
		return
	}
	if !ok {
		r.Keep("caches: beekeeper runs active")
		return
	}
	defer release()
	if m.refresh != nil {
		refs, builds, err := m.refresh()
		if err != nil {
			r.Errors = append(r.Errors, err)
			return
		}
		if builds {
			r.Keep("caches: builds running")
			return
		}
		m.refs = refs
	}
	// Also honor use locks held by older runs and manual cache clear go.
	if m.cache != "" {
		releaseGo, ok, err := gocache.TryTrim(m.cache)
		if err != nil {
			r.Errors = append(r.Errors, err)
			return
		}
		if !ok {
			r.Keep("caches: Go cache held by a run or clear")
			return
		}
		defer releaseGo()
		if diskprune.InUse(m.cache, m.refs) {
			r.Keep("Go cache in use")
		} else {
			u, err := gocache.Scan(m.cache)
			if err != nil {
				r.Errors = append(r.Errors, err)
			} else if target, trim := diskCacheTarget(u.Bytes, a.cfg.Doctor.GoCacheMax(), pressure); !trim {
				r.Keep("Go cache under cap")
			} else if r.dry {
				left := u.Bytes
				for _, e := range u.Entries {
					if left <= target {
						break
					}
					r.Freed += e.Size
					r.Removed++
					left -= e.Size
				}
			} else {
				n, removed, err := gocache.Trim(u, target, func() bool { return false })
				r.Freed += n
				r.Removed += removed
				if err != nil {
					r.Errors = append(r.Errors, err)
				}
			}
		}
	}
	pruneSccache(r, m, int64(max(0, a.cfg.Disk.SccacheMaxGiB))<<30, pressure)
}

func pruneSccache(r *diskRun, m diskMachine, capBytes int64, pressure bool) {
	if m.sccache == "" {
		return
	}
	if diskprune.InUse(m.sccache, m.refs) {
		r.Keep("sccache in use")
	} else if _, trim := diskCacheTarget(worktrees.Size(m.sccache), capBytes, pressure); !trim {
		r.Keep("sccache under cap")
	} else {
		r.Add(diskprune.Children(m.sccache, time.Time{}, m.refs, r.dry))
	}
}

// diskCacheTarget preserves warm caches on healthy scheduled runs.
func diskCacheTarget(size, capBytes int64, pressure bool) (int64, bool) {
	if pressure {
		return 0, true
	}
	if capBytes <= 0 || size <= capBytes {
		return size, false
	}
	return capBytes * 3 / 4, true
}

// diskPrune runs on schedule and under pressure. All watches share the mark
// and lock; pressure retries are bounded to once per five minutes.
func (w *watcher) diskPrune(now time.Time, pressure bool) {
	if !w.diskPruneOn || !w.cfg.Disk.Auto() || !w.diskPruning.CompareAndSwap(false, true) {
		return
	}
	if now.Before(w.diskPruneNext) && !pressure {
		w.diskPruning.Store(false)
		return
	}
	d := *w.app
	d.now = now
	w.bg.Add(1)
	go func() {
		defer w.bg.Done()
		defer w.diskPruning.Store(false)
		schedule := flock.New(filepath.Join(w.store.Dir(), "disk-prune-schedule.lock"))
		ok, err := schedule.TryLock()
		if err != nil || !ok {
			w.diskPruneNext = now.Add(goCacheRetry)
			if err != nil {
				w.emitNow("disk-prune", "DISK PRUNE kept everything: %v", err)
			}
			return
		}
		defer func() { _ = schedule.Unlock() }()
		var mark worktreesMark
		_, _ = w.store.ReadFile(diskPruneFile, &mark)
		every := w.cfg.Disk.Every.Duration
		if every <= 0 {
			every = time.Hour
		}
		if pressure {
			every = min(every, goCacheRetry)
		}
		if now.Sub(mark.At) < every {
			w.diskPruneNext = mark.At.Add(every)
			return
		}
		r := d.pruneDisk(false, pressure, func() (diskMachine, error) { return liveDisk(&d) })
		if r.locked {
			w.diskPruneNext = now.Add(goCacheRetry)
			return
		}
		w.diskPruneNext = now.Add(every)
		_ = w.store.WriteFile(diskPruneFile, worktreesMark{At: now})
		w.emitNow("disk-prune", "DISK PRUNE %s", r)
	}()
}

func (a *app) diskCmd() *cobra.Command {
	c := &cobra.Command{Use: "disk", Short: "Reclaim unused caches, finished worktrees and stale run output"}
	var apply bool
	p := &cobra.Command{Use: "prune", Short: "Show reclaimable disk data; --apply reclaims it", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		a.now = time.Now()
		r := a.pruneDisk(!apply, false, func() (diskMachine, error) { return liveDisk(a) })
		_, err := fmt.Fprintln(a.out, r)
		return errors.Join(err, errors.Join(r.Errors...))
	}}
	p.Flags().BoolVar(&apply, "apply", false, "reclaim disposable data after checking live processes and work")
	c.AddCommand(p)
	return c
}
