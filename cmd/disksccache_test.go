package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// sccacheEntries writes n sccache entries of size bytes each, entry i used
// i hours after the oldest, in sccache's two-level layout.
func sccacheEntries(t *testing.T, dir string, n, size int) []string {
	t.Helper()
	oldest := time.Now().Add(-time.Duration(n+1) * time.Hour)
	paths := make([]string, n)
	for i := range n {
		p := filepath.Join(dir, fmt.Sprintf("%x", i%16), fmt.Sprintf("%x", i/16%16), fmt.Sprintf("entry%04d", i))
		writeT(t, p, strings.Repeat("x", size))
		used := oldest.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(p, used, used); err != nil {
			t.Fatal(err)
		}
		paths[i] = p
	}
	return paths
}

func TestSccacheOverCapTrimsLeastRecentlyUsedEntriesToThreeQuarters(t *testing.T) {
	m := diskMachine{sccache: filepath.Join(t.TempDir(), "sccache")}
	paths := sccacheEntries(t, m.sccache, 8, 1000)
	var r diskRun
	pruneSccache(&r, m, 4000, false)
	if len(r.Errors) > 0 {
		t.Fatalf("trim: %s", r)
	}
	// 8000 bytes over a 4000 cap leave at most 3000: the five oldest go.
	for i, p := range paths {
		if want := i >= 5; exists(p) != want {
			t.Fatalf("entry %d kept=%v, want %v (least recently used first): %s", i, exists(p), want, r)
		}
	}
	if r.Removed != 5 || r.Freed != 5000 {
		t.Fatalf("removed %d, freed %d: %s", r.Removed, r.Freed, r)
	}
}

func TestSccacheUnderCapKeepsAndPressureClearsAll(t *testing.T) {
	m := diskMachine{sccache: filepath.Join(t.TempDir(), "sccache")}
	paths := sccacheEntries(t, m.sccache, 4, 1000)
	var r diskRun
	pruneSccache(&r, m, 1<<30, false)
	if r.Removed != 0 || r.Kept["sccache under cap"] != 1 {
		t.Fatalf("under cap: %s", r)
	}
	r = diskRun{}
	pruneSccache(&r, m, 1<<30, true)
	for _, p := range paths {
		if exists(p) {
			t.Fatalf("pressure kept %s: %s", p, r)
		}
	}
	if left, _ := os.ReadDir(m.sccache); len(left) != 0 || !exists(m.sccache) {
		t.Fatalf("pressure left %d directories, or removed the cache root", len(left))
	}
}

func TestSccacheTrimStopsTheIdleServerFirst(t *testing.T) {
	m := diskMachine{sccache: filepath.Join(t.TempDir(), "sccache")}
	paths := sccacheEntries(t, m.sccache, 4, 1000)
	running, stopped := true, 0
	m.sccacheServers = func() []int {
		if running {
			return []int{4242}
		}
		return nil
	}
	m.stopSccache = func(pids []int) error {
		for _, p := range paths {
			if !exists(p) {
				t.Fatal("trimmed before the server stopped")
			}
		}
		stopped += len(pids)
		running = false
		return nil
	}
	var r diskRun
	pruneSccache(&r, m, 3000, false)
	if stopped != 1 || len(r.Errors) > 0 || exists(paths[0]) || !exists(paths[3]) {
		t.Fatalf("stopped %d: %s", stopped, r)
	}
}

func TestSccacheKeptWhileTheServerDoesNotStop(t *testing.T) {
	m := diskMachine{sccache: filepath.Join(t.TempDir(), "sccache")}
	paths := sccacheEntries(t, m.sccache, 4, 1000)
	m.sccacheServers = func() []int { return []int{4242} }
	m.stopSccache = func([]int) error { return fmt.Errorf("refused") }
	var r diskRun
	pruneSccache(&r, m, 1000, true)
	if r.Removed != 0 || r.Kept["sccache: server did not stop"] != 1 {
		t.Fatalf("kept: %s", r)
	}
	for _, p := range paths {
		if !exists(p) {
			t.Fatalf("lost %s while the server ran", p)
		}
	}
}

func TestSccacheTrimEndsWhenABuildStartsAServer(t *testing.T) {
	m := diskMachine{sccache: filepath.Join(t.TempDir(), "sccache")}
	paths := sccacheEntries(t, m.sccache, 300, 10)
	asked := 0
	m.sccacheServers = func() []int {
		asked++
		if asked > 2 { // the check before the trim and the trim's first: none
			return []int{4343}
		}
		return nil
	}
	var r diskRun
	pruneSccache(&r, m, 1<<30, true)
	if r.Kept["sccache: a build started"] != 1 || len(r.Errors) > 0 || r.Removed != 256 || !exists(paths[299]) {
		t.Fatalf("removed %d: %s", r.Removed, r)
	}
}

func TestSccacheDryRunNeitherStopsNorRemoves(t *testing.T) {
	m := diskMachine{sccache: filepath.Join(t.TempDir(), "sccache")}
	paths := sccacheEntries(t, m.sccache, 4, 1000)
	m.sccacheServers = func() []int { return []int{4242} }
	m.stopSccache = func([]int) error { t.Fatal("dry run stopped the server"); return nil }
	r := diskRun{dry: true}
	pruneSccache(&r, m, 1000, false)
	if r.Removed != 4 || r.Freed != 4000 {
		t.Fatalf("dry: %s", r)
	}
	for _, p := range paths {
		if !exists(p) {
			t.Fatal("dry run removed an entry")
		}
	}
}

// TestStopSccacheServersStopsARealServer starts a server of its own on a
// port of its own, when sccache is installed, and stops only it.
func TestStopSccacheServersStopsARealServer(t *testing.T) {
	bin, err := exec.LookPath("sccache")
	if err != nil {
		t.Skip("sccache not installed")
	}
	before := sccacheServers()
	t.Setenv("SCCACHE_DIR", filepath.Join(t.TempDir(), "sccache"))
	t.Setenv("SCCACHE_SERVER_PORT", "42291")
	t.Setenv("SCCACHE_IDLE_TIMEOUT", "120")
	if out, err := exec.Command(bin, "--start-server").CombinedOutput(); err != nil {
		t.Fatalf("start: %v: %s", err, out)
	}
	var mine []int
	for _, pid := range sccacheServers() {
		if !slices.Contains(before, pid) {
			mine = append(mine, pid)
		}
	}
	if len(mine) == 0 {
		t.Fatal("no new sccache server")
	}
	t.Cleanup(func() { proc.Terminate(mine, time.Second) })
	// The watch's environment need not name the server's port.
	t.Setenv("SCCACHE_SERVER_PORT", "")
	if err := stopSccacheServers(mine); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(mine, proc.Alive) {
		t.Fatal("server still running")
	}
}
