package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/diskprune"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// diskCache is one resolved cache root of a disk.caches policy.
type diskCache struct {
	name   string
	policy diskprune.Policy
	// command is mode command's cleaner, its words; empty for the others.
	command []string
}

// cacheCleanerTimeout bounds one tool's cleaner.
const cacheCleanerTimeout = 10 * time.Minute

// runCacheCleaner runs a cleaner in its cache root; tests replace it.
var runCacheCleaner = func(ctx context.Context, dir string, argv []string) error {
	exe, err := proc.LookPath(argv[0])
	if err != nil {
		return err
	}
	c := proc.Command(ctx, exe, argv[1:]...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, oneLine(string(out)))
	}
	return nil
}

// cacheReport is one cache root's outcome in the dry run's listing.
type cacheReport struct {
	name, root string
	diskprune.CacheResult
}

// diskCacheEntries are the disk step's cache policies: beekeeper's own
// caches as default entries, changed field by field by a configured entry of
// the same name, then the configured ones. Mode off drops an entry.
func (a *app) diskCacheEntries() []config.DiskCache {
	sccache := os.Getenv("SCCACHE_DIR")
	if sccache == "" {
		if d, err := os.UserCacheDir(); err == nil {
			sccache = filepath.Join(d, "sccache")
		}
	}
	gib := func(n int) float64 { return float64(max(0, n)) }
	entries := []config.DiskCache{
		{Name: config.DiskCacheGo, Path: goCacheDir(), Mode: config.DiskCacheModeGo, MaxGiB: gib(a.cfg.Doctor.GoCacheMaxGiB)},
		{Name: config.DiskCacheSccache, Path: sccache, Mode: config.DiskCacheModeSc, MaxGiB: gib(a.cfg.Disk.SccacheMaxGiB)},
		{Name: config.DiskCacheTmp, Path: filepath.Join(a.cfg.StateDir, "tmp"), Mode: config.DiskCacheEntries, OlderThan: config.Duration{Duration: time.Hour}},
	}
	for _, e := range a.cfg.Disk.Caches {
		i := slices.IndexFunc(entries[:3], func(b config.DiskCache) bool { return b.Name == e.Name })
		if i < 0 {
			entries = append(entries, e)
			continue
		}
		b := &entries[i]
		if e.Path != "" {
			b.Path = e.Path
		}
		if e.Mode != "" {
			b.Mode = e.Mode
		}
		if e.OlderThan.Duration > 0 {
			b.OlderThan = e.OlderThan
		}
		if e.MaxGiB > 0 {
			b.MaxGiB = e.MaxGiB
		}
		b.Regenerable = b.Regenerable || e.Regenerable
	}
	return slices.DeleteFunc(entries, func(e config.DiskCache) bool { return e.Mode == config.DiskCacheOff })
}

// builtinCache is the resolved entry of one of beekeeper's own caches; its
// Path is empty once it is off or handed to another mode.
func (a *app) builtinCache(name, mode string) config.DiskCache {
	for _, e := range a.diskCacheEntries() {
		if e.Name == name && e.Mode == mode {
			return e
		}
	}
	return config.DiskCache{Name: name}
}

// resolveDiskCaches expands the entries and files policies' globs into
// cache roots, each root once, the first policy naming it winning.
func resolveDiskCaches(entries []config.DiskCache) []diskCache {
	var out []diskCache
	seen := map[string]bool{}
	for _, e := range entries {
		if e.Path == "" || e.Mode != "" && e.Mode != config.DiskCacheEntries && e.Mode != config.DiskCacheFiles && e.Mode != config.DiskCacheCommand {
			continue
		}
		roots := []string{e.Path}
		if strings.ContainsAny(e.Path, "*?[") {
			roots, _ = filepath.Glob(e.Path)
		}
		for _, root := range roots {
			if seen[root] {
				continue
			}
			seen[root] = true
			c := diskCache{name: e.Name, policy: diskprune.Policy{Root: root, Files: e.Mode == config.DiskCacheFiles, OlderThan: e.OlderThan.Duration, MaxBytes: e.MaxBytes(), Regenerable: e.Regenerable}}
			if e.Mode == config.DiskCacheCommand {
				c.command = strings.Fields(e.Command)
			}
			out = append(out, c)
		}
	}
	for i := range out {
		for j := range out {
			if i != j && strings.HasPrefix(out[j].policy.Root, out[i].policy.Root+string(filepath.Separator)) {
				out[i].policy.Nested = append(out[i].policy.Nested, out[j].policy.Root)
			}
		}
	}
	return out
}

// pruneCacheRoots trims every resolved cache root.
func (a *app) pruneCacheRoots(r *diskRun, m diskMachine, pressure bool) {
	caches := slices.Clone(m.caches)
	for _, d := range m.tmp {
		caches = append(caches, diskCache{name: config.DiskCacheTmp, policy: diskprune.Policy{Root: d, OlderThan: time.Hour}})
	}
	var recheck func() ([]string, error)
	if m.refresh != nil {
		recheck = func() ([]string, error) { refs, _, err := m.refresh(); return refs, err }
	}
	for _, c := range caches {
		var res diskprune.CacheResult
		if len(c.command) > 0 {
			res = cleanCache(c, pressure, m.refs, m.protect, recheck, r.dry)
		} else {
			c.policy.Recheck = recheck
			res = diskprune.Cache(c.policy, a.now, pressure, m.refs, m.protect, r.dry)
		}
		r.Add(res.Result)
		r.caches = append(r.caches, cacheReport{name: c.name, root: c.policy.Root, CacheResult: res})
	}
}

// cleanCache runs a command policy's cleaner: once its root holds more than
// the budget, and under disk pressure; never on a root that is a symbolic
// link, holds protected state or that a process references, read again
// right before the run. A dry run says it would run; what that frees is the
// tool's to decide, so it counts nothing.
func cleanCache(c diskCache, pressure bool, refs, protect []string, recheck func() ([]string, error), dry bool) diskprune.CacheResult {
	var r diskprune.CacheResult
	root := c.policy.Root
	fi, err := os.Lstat(root)
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
		if q != "" && diskprune.InUse(root, []string{q}) {
			r.Keep("holds protected state")
			return r
		}
	}
	r.Held, r.Newest = diskprune.Measure(root)
	if !pressure && (c.policy.MaxBytes == 0 || r.Held <= c.policy.MaxBytes) {
		return r
	}
	if diskprune.InUse(root, refs) {
		r.Keep("in use")
		return r
	}
	if dry {
		r.Keep("cleaner would run")
		return r
	}
	if recheck != nil {
		if refs, err = recheck(); err != nil {
			r.Keep("process references unknown")
			return r
		}
		if diskprune.InUse(root, refs) {
			r.Keep("in use")
			return r
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), cacheCleanerTimeout)
	defer cancel()
	if err := runCacheCleaner(ctx, root, c.command); err != nil {
		r.Errors = append(r.Errors, fmt.Errorf("cache cleaner: %w", err))
	}
	after, _ := diskprune.Measure(root)
	if after < r.Held {
		r.Freed = r.Held - after
	}
	r.Removed++
	return r
}

// holding names the caches that still hold the most, for a pressure line
// that freed too little: name, size left and last write.
func (r diskRun) holding(n int) string {
	reports := slices.Clone(r.caches)
	slices.SortFunc(reports, func(a, b cacheReport) int {
		return cmp.Compare(b.Held-b.Freed, a.Held-a.Freed)
	})
	var parts []string
	for _, c := range reports {
		if len(parts) == n || c.Held-c.Freed < 1<<20 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s %s (last write %s)", c.name, sizeStr(c.Held-c.Freed), c.Newest.Format(time.DateOnly)))
	}
	return strings.Join(parts, ", ")
}

// listing is the command's per-cache detail: each root, what it holds, what
// the run freed or would free, and why it kept the rest.
func (r diskRun) listing() string {
	verb := freedVerb(r.dry)
	var b strings.Builder
	for _, c := range r.caches {
		if c.Held == 0 && c.Freed == 0 && len(c.Kept) == 0 && len(c.Errors) == 0 {
			continue
		}
		label := c.root
		if c.name != c.root && !strings.ContainsAny(c.name, "*?[") {
			label = c.name + " " + c.root
		}
		fmt.Fprintf(&b, "  %s: holds %s, %s %s in %d entries", label, sizeStr(c.Held), verb, sizeStr(c.Freed), c.Removed)
		var keeps []string
		for _, why := range sortedReasons(c.Kept) {
			keeps = append(keeps, fmt.Sprintf("%d %s", c.Kept[why], why))
		}
		if len(keeps) > 0 {
			fmt.Fprintf(&b, "; kept: %s", strings.Join(keeps, ", "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// uncoveredCaches lists the largest direct children of the user cache
// directory that no cache policy covers, at least minBytes, largest first.
func uncoveredCaches(base string, entries []config.DiskCache, minBytes int64, n int) []cacheReport {
	items, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var patterns []string
	for _, e := range entries {
		if e.Path != "" {
			patterns = append(patterns, e.Path)
		}
	}
	covered := func(p string) bool {
		for _, pat := range patterns {
			// The path itself, a parent or one of its children is a policy's.
			for q := p; q != filepath.Dir(q); q = filepath.Dir(q) {
				if ok, _ := filepath.Match(pat, q); ok {
					return true
				}
			}
			if strings.HasPrefix(pat, p+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	var out []cacheReport
	for _, it := range items {
		p := filepath.Join(base, it.Name())
		if !it.IsDir() || covered(p) {
			continue
		}
		size, newest := diskprune.Measure(p)
		if size < minBytes {
			continue
		}
		out = append(out, cacheReport{name: it.Name(), root: p, CacheResult: diskprune.CacheResult{Held: size, Newest: newest}})
	}
	slices.SortFunc(out, func(a, b cacheReport) int { return cmp.Compare(b.Held, a.Held) })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// freedVerb says what a run did with the bytes it counts.
func freedVerb(dry bool) string {
	if dry {
		return "would free"
	}
	return "freed"
}

// uncoveredText names the uncovered directories of a pressure run, by their
// name in the user cache directory, with size and last write.
func (r diskRun) uncoveredText() string {
	var parts []string
	for _, c := range r.uncovered {
		parts = append(parts, fmt.Sprintf("%s %s (last write %s)", c.name, sizeStr(c.Held), c.Newest.Format(time.DateOnly)))
	}
	return strings.Join(parts, ", ")
}

// holdingText is what a pressure run left holding the space: the caches
// holding the most and the uncovered directories.
func (r diskRun) holdingText() string {
	var parts []string
	if h := r.holding(3); h != "" {
		parts = append(parts, "caches "+h)
	}
	if u := r.uncoveredText(); u != "" {
		parts = append(parts, "not covered by disk.caches in the user cache directory: "+u)
	}
	return strings.Join(parts, "; ")
}
