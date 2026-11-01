package diskprune

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cacheFile writes size bytes at root/rel, last written age ago, and moves
// every directory between back as well.
func cacheFile(t *testing.T, root, rel string, size int, age time.Duration) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Repeat("x", size)), 0600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-age)
	for q := p; q != root; q = filepath.Dir(q) {
		if err := os.Chtimes(q, at, at); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

const day = 24 * time.Hour

func TestCacheEntriesOlderThanKeepsYoungUsedCheckoutsAndLinks(t *testing.T) {
	root := t.TempDir()
	cacheFile(t, root, "old/a", 1000, 10*day)
	cacheFile(t, root, "fresh/a", 1000, day)
	cacheFile(t, root, "mixed/old", 1000, 10*day)
	cacheFile(t, root, "mixed/new", 10, time.Minute)
	cacheFile(t, root, "work/.git/HEAD", 10, 10*day)
	cacheFile(t, root, "work/main.go", 1000, 10*day)
	used := cacheFile(t, root, "used/a", 1000, 10*day)
	target := t.TempDir()
	cacheFile(t, target, "x", 5000, 10*day)
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	p := Policy{Root: root, OlderThan: 7 * day}
	dry := Cache(p, time.Now(), false, []string{used}, nil, true)
	if dry.Freed != 1000 || dry.Removed != 1 || dry.Held != 5010 || !exists(filepath.Join(root, "old")) {
		t.Fatalf("dry run: %+v", dry)
	}
	r := Cache(p, time.Now(), false, []string{used}, nil, false)
	if r.Freed != 1000 || r.Removed != 1 || len(r.Errors) > 0 {
		t.Fatalf("prune: %+v", r)
	}
	if exists(filepath.Join(root, "old")) {
		t.Fatal("old entry stayed")
	}
	for _, kept := range []string{"fresh", "mixed/old", "work/main.go", "used/a", "link", filepath.Join(target, "x")} {
		if !exists(filepath.Join(root, kept)) && !exists(kept) {
			t.Errorf("removed %s", kept)
		}
	}
	for _, why := range []string{"in use", "contains a checkout", "symbolic link", "written within 1h0m0s"} {
		if r.Kept[why] != 1 {
			t.Errorf("kept %q %d times: %v", why, r.Kept[why], r.Kept)
		}
	}
	if r.Held != 5010 { // held before the run: each entry, a checkout without its .git
		t.Errorf("held %d", r.Held)
	}
}

func TestCacheEntriesBudgetTrimsOldestFirstButNotTheHour(t *testing.T) {
	root := t.TempDir()
	cacheFile(t, root, "a/f", 1000, 3*day)
	cacheFile(t, root, "b/f", 1000, 2*day)
	cacheFile(t, root, "c/f", 1000, day)
	cacheFile(t, root, "d/f", 3000, time.Minute)
	r := Cache(Policy{Root: root, MaxBytes: 4500}, time.Now(), false, nil, nil, false)
	// 6000 held: a and b go (4000 left), c stays; d is too young to count.
	if r.Freed != 2000 || exists(filepath.Join(root, "a")) || exists(filepath.Join(root, "b")) || !exists(filepath.Join(root, "c")) {
		t.Fatalf("budget: %+v", r)
	}
	r = Cache(Policy{Root: root, MaxBytes: 1}, time.Now(), false, nil, nil, false)
	if !exists(filepath.Join(root, "d", "f")) || exists(filepath.Join(root, "c")) {
		t.Fatalf("young entry trimmed or old kept: %+v", r)
	}
}

func TestCacheFilesModeTrimsFilesAndKeepsAllOnReferenceOrCheckout(t *testing.T) {
	root := t.TempDir()
	cacheFile(t, root, "x/y/old", 1000, 9*day)
	cacheFile(t, root, "x/new", 1000, 2*day)
	r := Cache(Policy{Root: root, Files: true, OlderThan: 7 * day}, time.Now(), false, nil, nil, false)
	if r.Freed != 1000 || exists(filepath.Join(root, "x", "y")) || !exists(filepath.Join(root, "x", "new")) {
		t.Fatalf("files: %+v", r)
	}
	ref := cacheFile(t, root, "z/old", 1000, 9*day)
	if r := Cache(Policy{Root: root, Files: true, OlderThan: time.Hour}, time.Now(), false, []string{ref}, nil, false); r.Freed != 0 || r.Kept["in use"] != 1 {
		t.Fatalf("referenced cache trimmed: %+v", r)
	}
	cacheFile(t, root, "repo/.git/config", 10, 9*day)
	if r := Cache(Policy{Root: root, Files: true, OlderThan: time.Hour}, time.Now(), false, nil, nil, false); r.Freed != 0 || r.Kept["contains a checkout"] != 1 {
		t.Fatalf("cache with a checkout trimmed: %+v", r)
	}
}

func TestCachePressureDropsRegenerableOnly(t *testing.T) {
	root := t.TempDir()
	cacheFile(t, root, "a/f", 1000, 2*day)
	cacheFile(t, root, "b/f", 1000, time.Minute)
	p := Policy{Root: root, OlderThan: 7 * day}
	if r := Cache(p, time.Now(), true, nil, nil, true); r.Freed != 0 {
		t.Fatalf("pressure trimmed a cache not regenerable: %+v", r)
	}
	p.Regenerable = true
	if r := Cache(p, time.Now(), false, nil, nil, true); r.Freed != 0 {
		t.Fatalf("regenerable trimmed without pressure: %+v", r)
	}
	if r := Cache(p, time.Now(), true, nil, nil, false); r.Freed != 1000 || !exists(filepath.Join(root, "b", "f")) {
		t.Fatalf("pressure: %+v", r)
	}
}

func TestCacheRefusesProtectedRootsCheckoutsAndLinks(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, "state")
	cacheFile(t, state, "data", 1000, 30*day)
	cacheFile(t, home, "other/f", 1000, 30*day)
	if r := Cache(Policy{Root: home, OlderThan: time.Hour}, time.Now(), false, nil, []string{state}, false); r.Freed != 0 || r.Kept["holds protected state"] != 1 {
		t.Fatalf("protected root pruned: %+v", r)
	}
	repo := filepath.Join(home, "repo")
	cacheFile(t, repo, ".git/HEAD", 10, 30*day)
	cacheFile(t, repo, "src/f", 1000, 30*day)
	if r := Cache(Policy{Root: repo, OlderThan: time.Hour}, time.Now(), false, nil, nil, false); r.Freed != 0 || !exists(filepath.Join(repo, "src", "f")) {
		t.Fatalf("checkout root pruned: %+v", r)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(filepath.Join(home, "other"), link); err != nil {
		t.Fatal(err)
	}
	if r := Cache(Policy{Root: link, OlderThan: time.Hour}, time.Now(), false, nil, nil, false); r.Freed != 0 || !exists(filepath.Join(home, "other", "f")) {
		t.Fatalf("linked root pruned: %+v", r)
	}
	if r := Cache(Policy{Root: filepath.Join(home, "missing"), OlderThan: time.Hour}, time.Now(), false, nil, nil, false); len(r.Errors) > 0 || len(r.Kept) > 0 {
		t.Fatalf("missing root: %+v", r)
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
