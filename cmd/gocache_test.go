package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/config"
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

func TestTrimGoCacheWaitsWhileAGoBuildRuns(t *testing.T) {
	a, cache := goCacheApp(t)
	r := a.trimGoCache(false, func() []int { return []int{4242} })
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
