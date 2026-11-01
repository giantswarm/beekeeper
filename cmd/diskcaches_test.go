package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// staleCache writes an application cache whose entries were last written
// days ago, as the disk step met it: 0 freed while it filled the disk.
func staleCache(t *testing.T, dir string, entries ...string) {
	t.Helper()
	old := time.Now().Add(-9 * 24 * time.Hour)
	for _, e := range entries {
		p := filepath.Join(dir, e, "variant")
		writeT(t, p, strings.Repeat("x", 1<<20))
		_ = os.Chtimes(p, old, old)
		_ = os.Chtimes(filepath.Dir(p), old, old)
	}
}

const testAppCache = "app"

func customCaches(a *app) []diskCache {
	return slices.DeleteFunc(resolveDiskCaches(a.diskCacheEntries()), func(c diskCache) bool { return c.name == config.DiskCacheTmp })
}

func TestDiskPruneFreesZeroUntilACachePolicyNamesTheCache(t *testing.T) {
	a, _ := diskFixture(t)
	base := t.TempDir()
	app := filepath.Join(base, testAppCache)
	staleCache(t, app, "a", "b")
	m := diskMachine{}
	read := func() (diskMachine, error) { return m, nil }

	// Today's case: nothing names the cache, so the step frees nothing.
	if r := a.pruneDisk(true, true, read); r.Freed != 0 {
		t.Fatalf("freed without a policy: %s", r)
	}

	a.cfg.Disk.Caches = []config.DiskCache{{Name: testAppCache, Path: app, OlderThan: config.Duration{Duration: 7 * 24 * time.Hour}}}
	m.caches = customCaches(a)
	r := a.pruneDisk(true, false, read)
	if r.Freed != 2<<20 || r.Removed != 2 || !exists(filepath.Join(app, "a")) {
		t.Fatalf("dry run: %s", r)
	}
	if l := r.listing(); !strings.Contains(l, testAppCache+" "+app+": holds 2 MiB, would free 2 MiB in 2 entries") {
		t.Fatalf("listing=%q", l)
	}
	if roots := resolveDiskCaches([]config.DiskCache{{Name: "outer", Path: base}, {Name: testAppCache, Path: app}}); !slices.Equal(roots[0].policy.Nested, []string{app}) || roots[1].policy.Nested != nil {
		t.Fatalf("nested roots: %+v", roots)
	}
	r = a.pruneDisk(false, false, read)
	if r.Freed != 2<<20 || exists(filepath.Join(app, "a")) || exists(filepath.Join(app, "b")) || !exists(app) {
		t.Fatalf("apply: %s", r)
	}
}

func TestDiskCachePoliciesGlobBudgetAndReferences(t *testing.T) {
	a, _ := diskFixture(t)
	base := t.TempDir()
	staleCache(t, filepath.Join(base, "one-cache"), "a", "b", "c")
	staleCache(t, filepath.Join(base, "two-cache"), "a")
	a.cfg.Disk.Caches = []config.DiskCache{{Name: "apps", Path: filepath.Join(base, "*-cache"), MaxGiB: 1.5 / 1024}}
	held := filepath.Join(base, "one-cache", "c", "variant")
	m := diskMachine{caches: customCaches(a), refs: []string{held}}
	if len(m.caches) != 2 {
		t.Fatalf("glob roots: %+v", m.caches)
	}
	r := a.pruneDisk(false, false, func() (diskMachine, error) { return m, nil })
	// one-cache holds 3 MiB over a 1.5 MiB budget: its two oldest free
	// entries go, the used one stays; two-cache fits.
	if r.Freed != 2<<20 || !exists(held) || !exists(filepath.Join(base, "two-cache", "a")) || r.Kept["in use"] != 1 {
		t.Fatalf("budget: %s", r)
	}
}

func TestDiskCacheUnknownReferencesKeepPolicies(t *testing.T) {
	a, _ := diskFixture(t)
	app := filepath.Join(t.TempDir(), testAppCache)
	staleCache(t, app, "a")
	a.cfg.Disk.Caches = []config.DiskCache{{Name: testAppCache, Path: app, Regenerable: true}}
	m := diskMachine{caches: customCaches(a), unknown: &proc.UnknownError{Processes: []proc.UnknownProcess{{PID: 1, Err: os.ErrPermission}}}}
	r := a.pruneDisk(false, true, func() (diskMachine, error) { return m, nil })
	if r.Freed != 0 || !exists(filepath.Join(app, "a")) || r.Kept["caches and tmp: "+keptReferencesUnknown] != 1 {
		t.Fatalf("unknown references: %s", r)
	}
}

func TestDiskCacheBuiltinsAreDefaultEntries(t *testing.T) {
	a, _ := stubApp(t)
	names := func() []string {
		var n []string
		for _, e := range a.diskCacheEntries() {
			n = append(n, e.Name)
		}
		return n
	}
	if got := names(); !slices.Equal(got, []string{config.DiskCacheGo, config.DiskCacheSccache, config.DiskCacheTmp}) {
		t.Fatalf("defaults=%v", got)
	}
	if got := a.builtinCache(config.DiskCacheGo, config.DiskCacheModeGo).MaxBytes(); got != a.cfg.Doctor.GoCacheMax() {
		t.Fatalf("go cap=%d", got)
	}
	a.cfg.Disk.Caches = []config.DiskCache{
		{Name: config.DiskCacheGo, MaxGiB: 1},
		{Name: config.DiskCacheSccache, Mode: config.DiskCacheOff},
		{Name: config.DiskCacheTmp, OlderThan: config.Duration{Duration: 2 * time.Hour}},
		{Name: "uv", Path: "/cache/uv", MaxGiB: 2},
	}
	if got := names(); !slices.Equal(got, []string{config.DiskCacheGo, config.DiskCacheTmp, "uv"}) {
		t.Fatalf("configured=%v", got)
	}
	if got := a.builtinCache(config.DiskCacheGo, config.DiskCacheModeGo).MaxBytes(); got != 1<<30 {
		t.Fatalf("go cap=%d", got)
	}
	if a.builtinCache(config.DiskCacheSccache, config.DiskCacheModeSc).Path != "" {
		t.Fatal("sccache off still trimmed")
	}
	if tmp := resolveDiskCaches(a.diskCacheEntries())[0]; tmp.name != config.DiskCacheTmp || tmp.policy.OlderThan != 2*time.Hour {
		t.Fatalf("tmp=%+v", tmp)
	}
}

func TestDiskPrunePressureNamesWhatHoldsTheSpace(t *testing.T) {
	a, _ := diskFixture(t)
	app := filepath.Join(t.TempDir(), testAppCache)
	staleCache(t, app, "a")
	a.cfg.Disk.Caches = []config.DiskCache{{Name: testAppCache, Path: app, OlderThan: config.Duration{Duration: 30 * 24 * time.Hour}}}
	m := diskMachine{caches: customCaches(a)}
	r := a.pruneDisk(true, true, func() (diskMachine, error) { return m, nil })
	if r.Freed != 0 || !strings.Contains(r.String(), "caches holding most: app 1 MiB (last write ") {
		t.Fatalf("pressure line: %s", r)
	}
}

func TestUncoveredCachesListsWhatNoPolicyNames(t *testing.T) {
	base := t.TempDir()
	staleCache(t, filepath.Join(base, "covered"), "a")
	staleCache(t, filepath.Join(base, "parent", "child"), "a")
	staleCache(t, filepath.Join(base, "big"), "a", "b")
	staleCache(t, filepath.Join(base, "small"), "a")
	entries := []config.DiskCache{{Path: filepath.Join(base, "covered")}, {Path: filepath.Join(base, "parent", "*")}}
	got := uncoveredCaches(base, entries, 1<<20, 10)
	if len(got) != 2 || got[0].name != "big" || got[0].Held != 2<<20 || got[1].name != "small" {
		t.Fatalf("uncovered=%+v", got)
	}
	if got := uncoveredCaches(base, entries, 2<<20, 10); len(got) != 1 {
		t.Fatalf("min size: %+v", got)
	}
}
