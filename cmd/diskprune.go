package cmd

import (
	"context"
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
	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/diskprune"
	"github.com/giantswarm/beekeeper/internal/gocache"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/watch"
	"github.com/giantswarm/beekeeper/internal/worktrees"
)

const diskPruneFile = "disk-prune.json"
const diskPruneLock = "disk-prune.lock"

// diskHoldingFile records what a pressure run left holding the space, for
// the low-disk note that follows it.
const diskHoldingFile = "disk-holding.json"

// diskHoldingMin is the smallest uncovered directory a pressure run names.
var diskHoldingMin int64 = 100 << 20

// keptReferencesUnknown keeps a candidate decided while some process's
// references could not be read.
const (
	keptReferencesUnknown   = "process references unknown"
	keptCachesBuildsRunning = "caches: builds running"
	keptCachesUnknown       = "caches: " + keptReferencesUnknown
)

// unknownReferences reports a scan that read every process but some: its
// references are known for all but those, so only the step it gates keeps.
func unknownReferences(err error) bool {
	var unknown *proc.UnknownError
	return errors.As(err, &unknown)
}

// diskMachine is a snapshot of live references and disposable disk places.
// Tests replace the reader, so automatic pruning never reaches real data.
type diskMachine struct {
	refs           []string
	builds         bool
	pressure       bool
	cache, sccache string
	tmp, trash     []string
	// caches are the disk.caches policies' roots; protect are the paths no
	// cache root may hold (the home, beekeeper's state, checkout roots).
	caches  []diskCache
	protect []string
	// entries are the policies, cacheHome the user cache directory: a
	// pressure run that leaves the disk low (stillLow; nil, it stays low)
	// names what there no policy covers.
	entries   []config.DiskCache
	cacheHome string
	stillLow  func() bool
	trees     worktreeMachine
	refresh   func() (diskRefs, error)
	// foreign counts the read's skipped processes of other namespaces.
	foreign int
	// unknown is the read's unreadable processes; without refresh, every
	// step keeps its candidates while it is set.
	unknown error
	// sccacheServers lists the user's sccache processes; stopSccache stops
	// them before a trim. Nil, no server runs.
	sccacheServers func() []int
	stopSccache    func([]int) error
}

var liveDisk = (*app).diskMachine

// diskRefs is one read of the user's process references.
type diskRefs struct {
	refs   []string
	builds bool
	// foreign counts the unreadable processes skipped as other
	// namespaces' (proc.Scan.Foreign).
	foreign int
}

func diskReferences() (diskRefs, error) {
	scan, err := proc.UserScan(os.Getuid())
	if err != nil && !unknownReferences(err) {
		return diskRefs{}, err
	}
	d := diskRefs{foreign: len(scan.Foreign)}
	for _, comm := range scan.Commands {
		if slices.Contains([]string{"go", "compile", "link", "cargo", "rustc", "cc", "gcc", "clang", "ninja", "make", "cmake"}, comm) {
			d.builds = true
		}
	}
	slices.Sort(scan.Refs)
	d.refs = slices.Compact(scan.Refs)
	return d, err
}

func (a *app) diskMachine() (diskMachine, error) {
	entries := a.diskCacheEntries()
	m := diskMachine{refresh: diskReferences, sccacheServers: sccacheServers, stopSccache: stopSccacheServers, cache: a.builtinCache(config.DiskCacheGo, config.DiskCacheModeGo).Path, sccache: a.builtinCache(config.DiskCacheSccache, config.DiskCacheModeSc).Path, caches: resolveDiskCaches(entries), trash: a.cfg.Disk.TrashDirs}
	home, _ := os.UserHomeDir()
	m.protect = append([]string{home, a.cfg.StateDir, a.store.Dir()}, a.cfg.Disk.Checkouts...)
	m.protect = append(m.protect, a.cfg.Doctor.WorktreeRoots...)
	m.entries = entries
	m.cacheHome, _ = os.UserCacheDir()
	m.stillLow = func() bool {
		d, err := machine.ReadDisk(a.cfg.StateDir)
		return err != nil || d.FreeMiB < a.cfg.Watch.DiskMin(d.TotalMiB)
	}
	if d, err := machine.ReadDisk(a.cfg.StateDir); err == nil {
		m.pressure = d.FreeMiB < a.cfg.Watch.DiskMin(d.TotalMiB)
	}
	d, err := diskReferences()
	m.refs, m.builds, m.foreign = d.refs, d.builds, d.foreign
	// Conversation use is durable even between harness turns. Refresh these
	// references with each process scan rather than relying on open files.
	m.refresh = func() (diskRefs, error) {
		d, err := diskReferences()
		if err != nil {
			return d, err
		}
		st, err := a.store.Read()
		if err != nil {
			return d, err
		}
		d.refs = a.protectHarnessStores(st, d.refs)
		return d, nil
	}
	if unknownReferences(err) {
		m.unknown = err
	} else if err != nil {
		return m, fmt.Errorf("disk pruning kept everything: process references unavailable: %w", err)
	}
	m.trees, err = liveWorktrees(a)
	if err != nil {
		return m, err
	}
	roots := append(slices.Clone(a.cfg.Doctor.WorktreeRoots), filepath.Join(a.cfg.StateDir, "worktrees"))
	places := append(slices.Clone(a.cfg.Disk.Checkouts), a.cfg.Hooks.Scope.Dirs...)
	places = append(places, a.cfg.Projects.Dir)
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
	m.trees.recheck = func() ([]string, error) { d, err := diskReferences(); return d.refs, err }
	m.trees.unknown = m.unknown
	return m, nil
}

type diskRun struct {
	diskprune.Result
	trees  worktreeRun
	locked bool
	dry    bool
	// pressure runs name what still holds the space.
	pressure  bool
	docker    *dockerRun
	caches    []cacheReport
	uncovered []cacheReport
	// unknown names the processes whose references kept candidates.
	unknown error
	// foreign counts the last read's skipped processes of other namespaces.
	foreign int
}

func (r diskRun) String() string {
	if r.locked {
		return "disk: kept everything (another disk prune runs)"
	}
	verb := freedVerb(r.dry)
	parts := []string{fmt.Sprintf("disk: %s %s in %d cache/output/tmp/trash entries", verb, sizeStr(r.Freed+r.trees.freed()), r.Removed)}
	if r.docker != nil {
		parts = append(parts, r.docker.String())
	}
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
	if r.foreign > 0 {
		parts = append(parts, fmt.Sprintf("skipped %d unreadable processes of other namespaces", r.foreign))
	}
	if h := r.holding(3); r.pressure && h != "" {
		parts = append(parts, "caches holding most: "+h)
	}
	if u := r.uncoveredText(); u != "" {
		parts = append(parts, "still low; not covered by disk.caches: "+u)
	}
	if r.unknown != nil {
		parts = append(parts, "references unknown: "+r.unknown.Error())
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
	if a.context().Err() != nil {
		return r
	}
	m, err := read()
	if err != nil {
		r.Errors = append(r.Errors, err)
		return r
	}
	if a.context().Err() != nil {
		return r
	}
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
	pressure = pressure || m.pressure
	r.pressure = pressure
	r.foreign = m.foreign
	if pressure {
		docker := a.pruneDockerForPressure(dry, true, a.dockerHolds)
		r.Freed += max(int64(0), docker.before.Images-docker.after.Images) + max(int64(0), docker.before.BuildCache-docker.after.BuildCache)
		if docker.err != nil {
			r.Errors = append(r.Errors, fmt.Errorf("docker: %w", docker.err))
		}
		if docker.notable() {
			r.docker = &docker
		}
	}
	if a.context().Err() != nil {
		return r
	}
	a.pruneDiskCaches(&r, m, pressure)
	if a.context().Err() != nil {
		return r
	}
	known, ok := rereadDisk(&r, &m)
	if !ok {
		return r
	}
	if known {
		a.pruneCacheRoots(&r, m, pressure)
		a.pruneAbandonedStaging(&r, m)
	} else {
		r.Keep("caches and tmp: " + keptReferencesUnknown)
	}
	if a.context().Err() != nil {
		return r
	}
	if pressure {
		a.pruneFinishedState(&r, &m)
	}
	if a.context().Err() != nil {
		return r
	}
	m.trees.automatic = true
	m.trees.pressure = pressure
	m.trees.refs = m.refs
	m.trees.unknown = m.unknown
	m.trees.trash = filepath.Join(a.store.Dir(), "trash")
	// Worktree discovery and remote publication checks have their own lock.
	// Do not monopolize the disk lock while waiting on the network.
	if err := lock.Unlock(); err != nil {
		r.Errors = append(r.Errors, err)
		return r
	}
	r.trees = a.trimWorktrees(dry, func() (worktreeMachine, error) { return m.trees, nil })
	if a.context().Err() != nil {
		return r
	}
	ok, err = lock.TryLock()
	if err != nil || !ok {
		if err != nil {
			r.Errors = append(r.Errors, err)
		}
		return r
	}
	known, ok = rereadDisk(&r, &m)
	if !ok {
		return r
	}
	if known {
		cutoff := a.now.Add(-time.Hour)
		if pressure {
			cutoff = time.Time{}
		}
		trash := append(slices.Clone(m.trash), m.trees.trash)
		slices.Sort(trash)
		for _, d := range slices.Compact(trash) {
			if a.context().Err() != nil {
				return r
			}
			r.Add(diskprune.Children(d, cutoff, m.refs, dry))
		}
	} else {
		r.Keep("trash: " + keptReferencesUnknown)
	}
	if r.trees.err != nil {
		r.Errors = append(r.Errors, r.trees.err)
	}
	if len(r.trees.failed) > 0 {
		r.Errors = append(r.Errors, fmt.Errorf("%d worktree actions failed", len(r.trees.failed)))
	}
	if pressure && m.stillLow != nil && m.stillLow() {
		r.Keep("disk remains below floor after safe reclamation; starts stay held")
	}
	if pressure && m.cacheHome != "" && (m.stillLow == nil || m.stillLow()) && a.context().Err() == nil {
		r.uncovered = uncoveredCaches(m.cacheHome, m.entries, diskHoldingMin, 5)
	}
	if !dry && a.context().Err() == nil {
		_ = a.store.Log(event(watch.WatchParty, "disk.prune", "%s", r))
		if pressure {
			_ = a.store.WriteFile(diskHoldingFile, diskHolding{At: a.now, Text: r.holdingText()})
		}
	}
	return r
}

// rereadDisk refreshes m's references before a step. known is false while
// a process's references are unknown: that step keeps its candidates and
// the next step reads again. ok is false when the process table itself is
// unreadable, which ends the run.
func rereadDisk(r *diskRun, m *diskMachine) (known, ok bool) {
	if m.refresh == nil {
		if m.unknown != nil {
			r.unknown = m.unknown
		}
		return m.unknown == nil, true
	}
	d, err := m.refresh()
	if err != nil && !unknownReferences(err) {
		r.Errors = append(r.Errors, err)
		return false, false
	}
	m.refs, m.builds, m.foreign, m.unknown = d.refs, d.builds, d.foreign, err
	r.foreign = d.foreign
	if err != nil {
		r.unknown = err
	}
	return err == nil, true
}

// pruneAbandonedStaging reclaims crashed publication and source-fetch staging
// directories once they have been inactive for an hour and hold no live reference.
func (a *app) pruneAbandonedStaging(r *diskRun, m diskMachine) {
	var matches []string
	for _, pattern := range []string{
		filepath.Join(a.cfg.StateDir, executionPublications, "transfer-*"),
		filepath.Join(a.cfg.StateDir, executionSourcesDir, "*", "fetch-*"),
	} {
		found, _ := filepath.Glob(pattern)
		matches = append(matches, found...)
	}
	if len(matches) == 0 {
		return
	}
	slices.Sort(matches)
	live := diskprune.NewRefs(m.refs)
	cutoff := a.now.Add(-time.Hour)
	for _, path := range matches {
		if a.context().Err() != nil {
			return
		}
		r.Add(live.Output(path, cutoff, r.dry))
	}
}

func goCachePastMargin(dir string, capBytes int64) bool {
	if dir == "" || capBytes <= 0 {
		return false
	}
	u, err := gocache.Scan(dir)
	return err == nil && u.Bytes > goCacheForce(capBytes)
}

func holdCacheForRuns(parent context.Context, dir string) (func(), bool, error) {
	ctx, cancel := context.WithTimeout(parent, goCacheHoldWait)
	defer cancel()
	release, err := gocache.Hold(ctx, dir, func() {})
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return release, true, nil
}

func (a *app) pruneDiskCaches(r *diskRun, m diskMachine, pressure bool) {
	goCap := a.builtinCache(config.DiskCacheGo, config.DiskCacheModeGo).MaxBytes()
	force := pressure || goCachePastMargin(m.cache, goCap)
	if m.builds && !force {
		r.Keep(keptCachesBuildsRunning)
		return
	}
	buildCache := filepath.Join(a.store.Dir(), "build-cache")
	release, ok, err := gocache.TryTrim(buildCache)
	waited := false
	if err == nil && !ok && force && !r.dry {
		release, ok, err = holdCacheForRuns(a.context(), buildCache)
		waited = ok
	}
	if err != nil {
		r.Errors = append(r.Errors, err)
		return
	}
	if !ok {
		r.Keep("caches: beekeeper runs active")
		return
	}
	defer release()
	known, ok := rereadDisk(r, &m)
	if !ok {
		return
	}
	force = pressure || goCachePastMargin(m.cache, goCap)
	if m.builds && !force {
		r.Keep(keptCachesBuildsRunning)
		return
	}
	if !known {
		r.Keep(keptCachesUnknown)
		return
	}
	// Also honor use locks held by older runs and manual cache clear go.
	if m.cache != "" {
		releaseGo, ok, err := gocache.TryTrim(m.cache)
		waitedGo := false
		if err == nil && !ok && force && !r.dry {
			releaseGo, ok, err = holdCacheForRuns(a.context(), m.cache)
			waitedGo = ok
		}
		if err != nil {
			r.Errors = append(r.Errors, err)
			return
		}
		if !ok {
			r.Keep("caches: Go cache held by a run or clear")
			return
		}
		defer releaseGo()
		if waited || waitedGo {
			known, ok = rereadDisk(r, &m)
			if !ok {
				return
			}
			if !known {
				r.Keep(keptCachesUnknown)
				return
			}
		}
		if diskprune.InUse(m.cache, m.refs) {
			r.Keep("Go cache in use")
		} else {
			u, scanErr := gocache.Scan(m.cache)
			before := r.Freed
			if scanErr != nil {
				r.Errors = append(r.Errors, scanErr)
			} else if target, trim := diskCacheTarget(u.Bytes, goCap, pressure); !trim {
				r.Keep("Go cache under cap")
			} else if r.dry {
				dryTrim(r, u, target)
			} else {
				n, removed, trimErr := gocache.Trim(u, target, func() bool { return a.context().Err() != nil })
				r.Freed += n
				r.Removed += removed
				if trimErr != nil {
					r.Errors = append(r.Errors, trimErr)
				}
			}
			if scanErr == nil {
				recordBuiltin(r, config.DiskCacheGo, m.cache, u, before)
			}
		}
	}
	if a.context().Err() != nil {
		return
	}
	if m.builds {
		r.Keep(keptCachesBuildsRunning)
		return
	}
	pruneSccacheContext(a.context(), r, m, a.builtinCache(config.DiskCacheSccache, config.DiskCacheModeSc).MaxBytes(), pressure)
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

// watchDiskPrune runs on schedule and under pressure. All watches share the mark
// and lock; pressure retries are bounded to once per five minutes.
func watchDiskPrune(w *watch.Watcher, now time.Time, pressure bool) {
	if !w.DiskPruneOn || !watchApp(w.Host).cfg.Disk.Auto() || !w.DiskPruning.CompareAndSwap(false, true) {
		return
	}
	if now.Before(w.DiskPruneNext) && !pressure {
		w.DiskPruning.Store(false)
		return
	}
	d := *watchApp(w.Host)
	d.now = now
	w.Bg.Add(1)
	go func() {
		defer w.Bg.Done()
		defer w.DiskPruning.Store(false)
		schedule := flock.New(filepath.Join(watchApp(w.Host).store.Dir(), "disk-prune-schedule.lock"))
		ok, err := schedule.TryLock()
		if err != nil || !ok {
			w.DiskPruneNext = now.Add(goCacheRetry)
			if err != nil {
				w.EmitNow("disk-prune", "DISK PRUNE kept everything: %v", err)
			}
			return
		}
		defer func() { _ = schedule.Unlock() }()
		var mark worktreesMark
		_, _ = watchApp(w.Host).store.ReadFile(diskPruneFile, &mark)
		every := watchApp(w.Host).cfg.Disk.Every.Duration
		if every <= 0 {
			every = time.Hour
		}
		if pressure {
			every = min(every, goCacheRetry)
		}
		if now.Sub(mark.At) < every {
			w.DiskPruneNext = mark.At.Add(every)
			return
		}
		r := d.pruneDisk(false, pressure, func() (diskMachine, error) { return liveDisk(&d) })
		if d.context().Err() != nil {
			return
		}
		if r.locked {
			w.DiskPruneNext = now.Add(goCacheRetry)
			return
		}
		w.DiskPruneNext = now.Add(every)
		_ = watchApp(w.Host).store.WriteFile(diskPruneFile, worktreesMark{At: now})
		w.EmitNow("disk-prune", "DISK PRUNE %s", r)
	}()
}

func (a *app) diskCmd() *cobra.Command {
	c := &cobra.Command{Use: "disk", Short: "Reclaim unused caches, finished worktrees and stale run output"}
	var apply, dry, report bool
	p := &cobra.Command{Use: "prune", Short: "Show reclaimable disk data; --apply reclaims it", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		if apply && dry {
			return errors.New("--apply and --dry-run exclude each other")
		}
		a.now = clk.Now()
		r := a.pruneDisk(!apply, false, func() (diskMachine, error) { return liveDisk(a) })
		_, err := fmt.Fprint(a.out, r.String()+"\n"+r.listing())
		if report {
			err = errors.Join(err, a.reportUncovered())
		}
		return errors.Join(err, errors.Join(r.Errors...))
	}}
	p.Flags().BoolVar(&apply, "apply", false, "reclaim disposable data after checking live processes and work")
	p.Flags().BoolVar(&dry, "dry-run", false, "only list what each cache would free, with sizes (the default)")
	p.Flags().BoolVar(&report, "report", false, "also list the user cache directory's largest entries no disk.caches policy covers")
	c.AddCommand(p, a.diskHoldersCmd(), a.diskUsageCmd())
	return c
}

// recordBuiltin adds a built-in cache's line to the listing: what it held
// and what this run freed from it since before.
func recordBuiltin(r *diskRun, name, root string, u gocache.Usage, before int64) {
	c := cacheReport{name: name, root: root, CacheResult: diskprune.CacheResult{Held: u.Bytes}}
	if n := len(u.Entries); n > 0 {
		c.Newest = u.Entries[n-1].Used
	}
	c.Freed = r.Freed - before
	r.caches = append(r.caches, c)
}

// reportUncovered lists what of the user cache directory no policy covers,
// so disk.caches can be extended.
func (a *app) reportUncovered() error {
	base, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	list := uncoveredCaches(base, a.diskCacheEntries(), 100<<20, 15)
	if _, err := fmt.Fprintf(a.out, "not covered by disk.caches under %s (100 MiB or more):\n", base); err != nil {
		return err
	}
	for _, c := range list {
		if _, err := fmt.Fprintf(a.out, "  %s: %s, last write %s\n", c.root, sizeStr(c.Held), c.Newest.Format(time.DateOnly)); err != nil {
			return err
		}
	}
	return nil
}
