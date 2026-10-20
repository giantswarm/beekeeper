package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/gocache"
	"github.com/giantswarm/beekeeper/internal/state"
)

// goCacheApp is an app with a 2 GiB cap over a Go build cache of four
// sparse 1 GiB entries, the oldest last: 4 GiB in all.
func goCacheApp(t *testing.T) (*app, string) {
	t.Helper()
	cache := t.TempDir()
	t.Setenv("GOCACHE", cache)
	now := time.Now()
	for i, name := range []string{"00/new-a", "01/mid-d", "02/old-d", "03/oldest-d"} {
		p := filepath.Join(cache, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(p) //nolint:gosec // under the test's own directory
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(1 << 30); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		at := now.Add(-time.Duration(i*24) * time.Hour)
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{StateDir: dir, Doctor: config.Doctor{GoCacheMaxGiB: 2}}
	return &app{cfg: cfg, store: store, now: now, out: &bytes.Buffer{}}, cache
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func noBuilds() []int { return nil }

func TestTrimGoCacheRemovesTheLeastRecentlyUsedAndLogs(t *testing.T) {
	a, cache := goCacheApp(t)
	r := a.trimGoCache(false, noBuilds)
	if r.err != nil || r.removed != 3 || r.before != 4<<30 || r.after != 1<<30 {
		t.Fatalf("trim = %+v; want 3 removed, 4 GiB down to 1 GiB (under 3/4 of the 2 GiB cap)", r)
	}
	if !exists(filepath.Join(cache, "00/new-a")) || exists(filepath.Join(cache, "01/mid-d")) {
		t.Error("the newest entry went, or an older one stayed")
	}
	want := "Go build cache " + cache + ": 4.0 GiB, cap 2.0 GiB (doctor.goCacheMaxGiB); trimmed to 1.0 GiB, 3 entries removed least recently used first"
	if got := r.String(); got != want {
		t.Errorf("line =\n %q\nwant\n %q", got, want)
	}
	evs, err := a.store.Events(10, func(e state.Event) bool { return e.Verb == goCacheVerb })
	if err != nil || len(evs) != 1 || evs[0].Detail != want {
		t.Errorf("logged %+v, %v; want one %s event with the line", evs, err, goCacheVerb)
	}
}

// overCap puts the cache of goCacheApp under a 4 GiB cap with a fifth,
// newest, 512 MiB entry: over the cap, short of its margin.
func overCap(t *testing.T, a *app, cache string) {
	t.Helper()
	a.cfg.Doctor.GoCacheMaxGiB = 4
	addEntry(t, cache, "04/new-b", 1<<29, time.Now())
}

// addEntry writes a sparse cache entry of size bytes used at at.
func addEntry(t *testing.T, cache, name string, size int64, at time.Time) {
	t.Helper()
	p := filepath.Join(cache, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(p) //nolint:gosec // under the test's own directory
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

// building is a go build that always runs.
func building() []int { return []int{4242} }

func TestTrimGoCacheWaitsWhileAGoBuildRuns(t *testing.T) {
	a, cache := goCacheApp(t)
	overCap(t, a, cache)
	r := a.trimGoCache(false, building)
	if r.removed != 0 || !exists(filepath.Join(cache, "03/oldest-d")) {
		t.Fatalf("trim removed %d entries while a build ran", r.removed)
	}
	if !strings.Contains(r.String(), "its trim waits for the go builds (PIDs 4242)") || !r.notable() {
		t.Errorf("line = %q", r)
	}
}

func TestTrimGoCacheDryRunRemovesNothing(t *testing.T) {
	a, cache := goCacheApp(t)
	r := a.trimGoCache(true, noBuilds)
	if r.removed != 0 || !exists(filepath.Join(cache, "03/oldest-d")) {
		t.Fatal("a dry run removed entries")
	}
	if !strings.HasSuffix(r.String(), "would trim it to 1.5 GiB") {
		t.Errorf("line = %q", r)
	}
}

func TestTrimGoCacheLeavesItToTheTrimUnderWay(t *testing.T) {
	a, cache := goCacheApp(t)
	lock := flock.New(filepath.Join(a.store.Dir(), goCacheLock))
	if ok, err := lock.TryLock(); !ok || err != nil {
		t.Fatal(ok, err)
	}
	defer func() { _ = lock.Unlock() }()
	r := a.trimGoCache(false, noBuilds)
	if !r.locked || r.removed != 0 || !exists(filepath.Join(cache, "03/oldest-d")) {
		t.Fatalf("trim = %+v; want it left to the lock's holder", r)
	}
}

func TestTrimGoCacheUnderTheCapOrOffIsQuiet(t *testing.T) {
	a, _ := goCacheApp(t)
	a.cfg.Doctor.GoCacheMaxGiB = 8
	if r := a.trimGoCache(false, noBuilds); r.removed != 0 || r.notable() {
		t.Errorf("under the cap: %+v", r)
	}
	a.cfg.Doctor.GoCacheMaxGiB = -1
	if r := a.trimGoCache(false, noBuilds); r.max != 0 || r.before != 0 || r.notable() {
		t.Errorf("off: %+v", r)
	}
}

func TestTrimGoCachePastTheMarginTrimsWhileAGoBuildRuns(t *testing.T) {
	a, cache := goCacheApp(t)
	r := a.trimGoCache(false, building)
	if r.err != nil || !r.forced || r.removed != 3 || r.after != 1<<30 {
		t.Fatalf("trim = %+v; want 4 GiB, past 5/4 of the 2 GiB cap, trimmed to 1 GiB while the build runs", r)
	}
	if !exists(filepath.Join(cache, "00/new-a")) || exists(filepath.Join(cache, "03/oldest-d")) {
		t.Error("the newest entry went, or the oldest stayed")
	}
	want := "Go build cache " + cache + ": 4.0 GiB, cap 2.0 GiB (doctor.goCacheMaxGiB); trimmed to 1.0 GiB, 3 entries removed least recently used first while go builds run (PIDs 4242), past the cap by a quarter"
	if got := r.String(); got != want {
		t.Errorf("line =\n %q\nwant\n %q", got, want)
	}
	evs, err := a.store.Events(10, func(e state.Event) bool { return e.Verb == goCacheVerb })
	if err != nil || len(evs) != 1 || evs[0].Detail != want {
		t.Errorf("logged %+v, %v; want one %s event with the line", evs, err, goCacheVerb)
	}
}

// No trim, not even one past the margin, starts while a beekeeper run
// holds the cache: a compile reads its entries.
func TestTrimGoCacheWaitsForABeekeeperRun(t *testing.T) {
	a, cache := goCacheApp(t)
	release, err := gocache.Use(context.Background(), cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, builds := range []func() []int{noBuilds, building} {
		r := a.trimGoCache(false, builds)
		if r.removed != 0 || !exists(filepath.Join(cache, "03/oldest-d")) {
			t.Fatalf("trim removed %d entries while a beekeeper run held the cache", r.removed)
		}
		if !r.used || !strings.HasSuffix(r.String(), "its trim waits for the beekeeper runs that hold the cache") || !r.notable() {
			t.Errorf("trim = %+v, line %q", r, r)
		}
	}
	release()
	if r := a.trimGoCache(false, noBuilds); r.used || r.removed != 3 {
		t.Errorf("after the run: %+v; want the trim", r)
	}
}

// A desk where a go build always runs: the cache over its cap waits, the
// watch says GO CACHE with its size, cap and wait once it stayed over the
// cap across two intervals, and the trim runs once it is past the margin.
func TestGoCacheLookWithABuildAlwaysRunning(t *testing.T) {
	a, cache := goCacheApp(t)
	overCap(t, a, cache)
	out := &bytes.Buffer{}
	a.out = out
	a.cfg.Doctor.GoCacheEvery.Duration = time.Hour
	a.cfg.Watch.Repeat.Duration = time.Hour
	w := &watcher{app: a, last: map[string]time.Time{}}
	t0 := time.Now()
	for _, at := range []time.Duration{0, goCacheRetry, 55 * time.Minute} {
		w.goCacheLook(t0.Add(at), building)
	}
	if out.Len() != 0 {
		t.Fatalf("within one interval the watch said:\n%s", out)
	}
	if !exists(filepath.Join(cache, "03/oldest-d")) {
		t.Fatal("a cache short of the margin was trimmed while a build ran")
	}
	w.goCacheLook(t0.Add(time.Hour), building)
	want := "GO CACHE: Go build cache " + cache + ": 4.5 GiB, cap 4.0 GiB (doctor.goCacheMaxGiB); its trim waits for the go builds (PIDs 4242); over the cap for 1h0m0s"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("after two intervals the watch said:\n%s\nwant\n%s", out, want)
	}
	addEntry(t, cache, "05/new-c", 1<<30, time.Now())
	w.goCacheLook(t0.Add(time.Hour+goCacheRetry), building)
	if !strings.Contains(out.String(), "ENDED GO CACHE") {
		t.Errorf("past the margin the trim did not end GO CACHE:\n%s", out)
	}
	if exists(filepath.Join(cache, "03/oldest-d")) || !exists(filepath.Join(cache, "05/new-c")) {
		t.Error("past the margin the oldest entry stayed, or the newest went")
	}
	u, err := gocache.Scan(cache)
	if err != nil || u.Bytes >= 4<<30 {
		t.Errorf("the cache is %d bytes after the trim, %v; want under the 4 GiB cap", u.Bytes, err)
	}
	var m goCacheMark
	if _, err := a.store.ReadFile(goCacheFile, &m); err != nil || !m.Waiting.IsZero() {
		t.Errorf("mark = %+v, %v; want the wait cleared", m, err)
	}
}

// A clear waits for the beekeeper run that holds the cache, then clears
// it with go clean and logs the clear: the run's compile never misses an
// entry.
func TestClearGoCacheWaitsForABeekeeperRun(t *testing.T) {
	a, cache := goCacheApp(t)
	run, err := gocache.Use(context.Background(), cache)
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan string, 1)
	done := make(chan error, 1)
	var line string
	go func() {
		var err error
		line, err = a.clearGoCache(context.Background(), false, func(s string) { waiting <- s })
		done <- err
	}()
	if say := <-waiting; !strings.Contains(say, "waiting for the beekeeper runs that hold the Go build cache "+cache) {
		t.Errorf("said %q", say)
	}
	time.Sleep(500 * time.Millisecond)
	if !exists(filepath.Join(cache, "03/oldest-d")) {
		t.Fatal("the clear removed an entry while a beekeeper run held the cache")
	}
	run()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, e := range []string{"00/new-a", "03/oldest-d"} {
		if exists(filepath.Join(cache, e)) {
			t.Errorf("%s stayed after the clear", e)
		}
	}
	if !strings.HasPrefix(line, "Go build cache "+cache+" cleared: 4.0 GiB freed") {
		t.Errorf("line = %q", line)
	}
	evs, err := a.store.Events(10, func(e state.Event) bool { return e.Verb == goCacheClearVerb })
	if err != nil || len(evs) != 1 || evs[0].Detail != line {
		t.Errorf("logged %+v, %v; want one %s event with the line", evs, err, goCacheClearVerb)
	}
	again, err := gocache.Use(context.Background(), cache)
	if err != nil {
		t.Fatalf("a run after the clear: %v", err)
	}
	again()
}

func TestClearGoCacheModcache(t *testing.T) {
	a, _ := goCacheApp(t)
	mod := t.TempDir()
	t.Setenv("GOMODCACHE", mod)
	// The go command keeps the module cache read-only.
	pkg := filepath.Join(mod, "example.com", "m@v1.0.0")
	if err := os.MkdirAll(pkg, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "go.mod"), []byte("module example.com/m\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pkg, 0o500); err != nil { //nolint:gosec // read-only, as the go command leaves it
		t.Fatal(err)
	}
	line, err := a.clearGoCache(context.Background(), true, func(string) { t.Error("waited with no run") })
	if err != nil {
		t.Fatal(err)
	}
	if exists(pkg) || !strings.Contains(line, "; module cache "+mod+" cleared") {
		t.Errorf("module left: %v; line %q", exists(pkg), line)
	}
}

func TestClearGoCacheGivesUpAfterItsWait(t *testing.T) {
	a, cache := goCacheApp(t)
	run, err := gocache.Use(context.Background(), cache)
	if err != nil {
		t.Fatal(err)
	}
	defer run()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := a.clearGoCache(ctx, false, func(string) {}); err == nil || !strings.Contains(err.Error(), "nothing cleared (--wait)") {
		t.Fatalf("clear = %v", err)
	}
	if !exists(filepath.Join(cache, "03/oldest-d")) {
		t.Error("cleared after the wait ran out")
	}
}

// Inside a beekeeper run, which holds the cache, the clear would wait on
// its own run.
func TestClearGoCacheRefusedInsideARun(t *testing.T) {
	a, cache := goCacheApp(t)
	t.Setenv(gocache.HeldEnv, cache)
	if _, err := a.clearGoCache(context.Background(), false, func(string) { t.Error("waited") }); err == nil || !strings.Contains(err.Error(), "inside a beekeeper run") {
		t.Fatalf("clear = %v", err)
	}
	if !exists(filepath.Join(cache, "00/new-a")) {
		t.Error("cleared")
	}
}
