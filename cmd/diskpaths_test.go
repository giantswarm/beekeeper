package cmd

import (
	"cmp"
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

const diskGrowthTestPath = "/cache/b"

func TestScanDiskPaths(t *testing.T) {
	root := t.TempDir()
	for name, data := range map[string]string{"a/file": "1234", "a/deep/file": "123456", "b/file": "12"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	got := scanDiskPaths(context.Background(), []string{root}, 6, 100)
	top := topDiskPaths(got, false, 3)
	if len(top) != 2 || top[0].Bytes != 10 || top[1].Bytes != 2 || top[0].Partial {
		t.Fatalf("scan: %+v", top)
	}
	shallow := topDiskPaths(scanDiskPaths(context.Background(), []string{root}, 1, 100), false, 3)
	if shallow[0].Bytes != 4 || !shallow[0].Partial {
		t.Fatalf("depth bound: %+v", shallow)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := scanDiskPaths(ctx, []string{root}, 6, 100); len(got) != 0 {
		t.Fatalf("canceled: %+v", got)
	}
	limited := scanDiskPaths(context.Background(), []string{root}, 6, 2)
	if len(limited) > 1 || len(limited) == 1 && !limited[0].Partial {
		t.Fatalf("entry bound: %+v", limited)
	}
}

// A large directory spends its own share: its neighbours and the next root
// are still measured whole.
func TestScanDiskPathsSharesItsBudget(t *testing.T) {
	big, small := t.TempDir(), t.TempDir()
	for i := range 200 {
		path := filepath.Join(big, "a-large", "f"+strconv.Itoa(i))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(big, "b-small", "file"), filepath.Join(small, "cache", "file")} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("12"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]diskPath{}
	for _, p := range scanDiskPaths(context.Background(), []string{big, small}, 6, 40) {
		got[filepath.Base(p.Path)] = p
	}
	if !got["a-large"].Partial {
		t.Fatalf("large directory: %+v", got)
	}
	for _, name := range []string{"b-small", "cache"} {
		if p, ok := got[name]; !ok || p.Partial || p.Bytes != 2 {
			t.Fatalf("%s starved: %+v", name, got)
		}
	}
}

func TestScanDiskPathsSkipsPlacesWithTheirOwnRoot(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, "beekeeper")
	if err := os.MkdirAll(filepath.Join(state, "workspaces"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "workspaces", "file"), []byte("1234"), 0600); err != nil {
		t.Fatal(err)
	}
	got := scanDiskPaths(context.Background(), []string{home, state}, 6, 100, state)
	if len(got) != 1 || got[0].Path != filepath.Join(state, "workspaces") || got[0].Bytes != 4 {
		t.Fatalf("scan: %+v", got)
	}
}

func TestDiskPathRootsIncludeHarnessWorktrees(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "projects", "repo", ".claude", "worktrees")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{StateDir: filepath.Join(dir, "state"), Projects: config.Projects{Dir: filepath.Join(dir, "projects"), Worktrees: []string{filepath.Join(dir, "projects", "worktrees")}}}}
	got := a.diskPathRoots()
	for _, want := range []string{a.cfg.StateDir, a.cfg.Projects.Worktrees[0], nested} {
		if !slices.Contains(got, want) {
			t.Fatalf("roots %v lack %s", got, want)
		}
	}
}

func TestDiskUsageNamesOwners(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("SCCACHE_DIR", filepath.Join(dir, "cache", "sccache"))
	nested := filepath.Join(dir, "projects", "repo", ".claude", "worktrees", "tree")
	for path, size := range map[string]int{
		filepath.Join(dir, "state", "workspaces", "file"):     8,
		filepath.Join(dir, "cache", "othertool", "file"):      16,
		filepath.Join(dir, "cache", "sccache", "file"):        4,
		filepath.Join(nested, "file"):                         32,
		filepath.Join(dir, "projects", "worktrees", "a", "f"): 2,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, size), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a := &app{cfg: &config.Config{StateDir: filepath.Join(dir, "state"), Projects: config.Projects{Dir: filepath.Join(dir, "projects"), Worktrees: []string{filepath.Join(dir, "projects", "worktrees")}}}}
	got := map[string]string{}
	var sizes []int64
	for _, r := range a.diskUsage(context.Background(), time.Minute) {
		got[filepath.Base(r.Path)] = r.Owner
		sizes = append(sizes, r.Bytes)
	}
	want := map[string]string{"workspaces": pluginName, "othertool": "othertool (" + diskOwnerNoPolicy + ")", "sccache": "cache sccache", "tree": "harness worktree", "a": "worktree"}
	if !maps.Equal(got, want) {
		t.Fatalf("owners: %v, want %v", got, want)
	}
	if !slices.IsSortedFunc(sizes, func(x, y int64) int { return cmp.Compare(y, x) }) {
		t.Fatalf("not largest first: %v", sizes)
	}
}

func TestDiskPathGrowthAndReports(t *testing.T) {
	prev := []diskPath{{Path: "/cache/a", Bytes: 100}, {Path: diskGrowthTestPath, Bytes: 5}, {Path: "/cache/partial", Bytes: 1, Partial: true}}
	cur := []diskPath{{Path: "/cache/a", Bytes: 90}, {Path: diskGrowthTestPath, Bytes: 15}, {Path: "/cache/new", Bytes: 1000}, {Path: "/cache/partial", Bytes: 20}}
	diskPathGrowth(cur, prev)
	top := topDiskPaths(cur, true, 3)
	if len(top) != 1 || top[0].Path != diskGrowthTestPath || *top[0].Growth != 10 {
		t.Fatalf("growth: %+v", top)
	}
	lines := strings.Join(diskPathLines(cur), "\n")
	if !strings.Contains(lines, "disk paths by size:") || !strings.Contains(lines, "disk paths by growth: \"/cache/b\"") || !strings.Contains(lines, "growth unknown") {
		t.Fatal(lines)
	}
	dw := diskWatch{paths: cur}
	if line := dw.writersLine(time.Now(), 0, nil, nil); !strings.Contains(line, "fastest-growing path: \"/cache/b\"") {
		t.Fatal(line)
	}
	changes := strings.Join(diffSnapshots(&snapshot{DiskPaths: prev}, &snapshot{DiskPaths: cur}), "\n")
	if !strings.Contains(changes, "disk paths by growth") {
		t.Fatal(changes)
	}
	// An incomplete new reading must not produce an apparent increase.
	cur[1].Partial = true
	diskPathGrowth(cur, prev)
	if len(topDiskPaths(cur, true, 3)) != 0 {
		t.Fatal("partial scan reported growth")
	}
}
