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
	"github.com/giantswarm/beekeeper/internal/watch"
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
	if got := scanDiskPaths(ctx, []string{root}, 6, 100); len(got) != 1 || got[0].Path != root || !got[0].Partial {
		t.Fatalf("canceled: %+v", got)
	}
	limited := scanDiskPaths(context.Background(), []string{root}, 6, 2)
	if !slices.ContainsFunc(limited, func(p diskPath) bool { return p.Path == root && p.Partial }) {
		t.Fatalf("entry bound: %+v", limited)
	}
	if got := scanDiskPaths(context.Background(), []string{filepath.Join(root, "missing")}, 6, 100); len(got) != 0 {
		t.Fatalf("missing root: %+v", got)
	}
}

// The entries a root's listing takes can leave nothing to measure with: what
// was found still shows, as unread, and never as an empty inventory.
func TestScanDiskPathsShowsWhatStayedUnread(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("1"), 0600); err != nil {
		t.Fatal(err)
	}
	got := scanDiskPaths(context.Background(), []string{root}, 6, 2)
	if !slices.ContainsFunc(got, func(p diskPath) bool { return p.Path == filepath.Join(root, "dir") && p.Partial }) {
		t.Fatalf("unmeasured directory: %+v", got)
	}
}

// The state root's own files hold the audit log: they count, a large one by
// name and the rest together.
func TestScanDiskPathsCountsRootFiles(t *testing.T) {
	root := t.TempDir()
	log := filepath.Join(root, "events.jsonl")
	if err := os.WriteFile(log, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(log, diskPathFileMin); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte("123"), 0600); err != nil {
		t.Fatal(err)
	}
	got := scanDiskPaths(context.Background(), []string{root}, 6, 100)
	want := []diskPath{{Path: log, Bytes: diskPathFileMin}, {Path: filepath.Join(root, diskPathOtherFiles), Bytes: 3}}
	if !slices.Equal(got, want) {
		t.Fatalf("files: %+v", got)
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
	if err := os.MkdirAll(filepath.Join(state, "blobs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "blobs", "file"), []byte("1234"), 0600); err != nil {
		t.Fatal(err)
	}
	got := scanDiskPaths(context.Background(), []string{home, state}, 6, 100, state)
	if len(got) != 1 || got[0].Path != filepath.Join(state, "blobs") || got[0].Bytes != 4 {
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
	place := filepath.Join(dir, "projects", "worktrees")
	gitfile := "gitdir: /repository/.git/worktrees/tree\n"
	for path, content := range map[string]string{
		filepath.Join(dir, "state", "blobs", "file"):     "12345678",
		filepath.Join(dir, "state", "state.json"):        "12",
		filepath.Join(dir, "cache", "othertool", "file"): "1234",
		filepath.Join(dir, "cache", "sccache", "file"):   "123",
		filepath.Join(nested, ".git"):                    gitfile,
		filepath.Join(place, "repo", "a", ".git"):        gitfile,
		filepath.Join(place, "repo", "a", "big"):         strings.Repeat("x", 64),
		filepath.Join(place, "repo", "fix", "b", ".git"): gitfile,
		filepath.Join(place, "repo", "leftover", "file"): "1",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a := &app{cfg: &config.Config{StateDir: filepath.Join(dir, "state"), Projects: config.Projects{Dir: filepath.Join(dir, "projects"), Worktrees: []string{place}}}}
	got := map[string]string{}
	bytes := map[string]int64{}
	var sizes []int64
	for _, r := range a.diskUsage(context.Background(), time.Minute) {
		rel, err := filepath.Rel(dir, r.Path)
		if err != nil {
			t.Fatal(err)
		}
		got[rel], bytes[rel] = r.Owner, r.Bytes
		sizes = append(sizes, r.Bytes)
	}
	treeA, treeB := filepath.Join("projects", "worktrees", "repo", "a"), filepath.Join("projects", "worktrees", "repo", "fix", "b")
	placeRow := filepath.Join("projects", "worktrees", "repo")
	want := map[string]string{
		filepath.Join("state", "blobs"):                                   pluginName,
		filepath.Join("state", diskPathOtherFiles):                        pluginName,
		filepath.Join("cache", "othertool"):                               "othertool (" + diskOwnerNoPolicy + ")",
		filepath.Join("cache", "sccache"):                                 "cache sccache",
		filepath.Join("projects", "repo", ".claude", "worktrees", "tree"): diskOwnerHarness,
		treeA:    diskOwnerTree,
		treeB:    diskOwnerTree,
		placeRow: diskOwnerPlace,
	}
	if !maps.Equal(got, want) {
		t.Fatalf("owners: %v, want %v", got, want)
	}
	if n := int64(len(gitfile)); bytes[treeA] != n+64 || bytes[treeB] != n || bytes[placeRow] != 1 {
		t.Fatalf("sizes: %v", bytes)
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
	dw := watch.DiskWatch{Paths: cur}
	if line := dw.WritersLine(time.Now(), 0, nil, nil); !strings.Contains(line, "fastest-growing path: \"/cache/b\"") {
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
