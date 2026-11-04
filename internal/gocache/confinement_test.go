package gocache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestTrimDoesNotFollowReplacedShard(t *testing.T) {
	dir := cache(t, map[string][2]int{entryA: {100, 24}})
	outside := t.TempDir()
	other := filepath.Join(outside, filepath.Base(entryA))
	if err := os.WriteFile(other, []byte("outside data"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := trimLink(t, outside)
	u, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	freed, removed, err := Trim(u, 0, func() bool {
		shard := filepath.Join(dir, filepath.Dir(entryA))
		if err := os.Rename(shard, filepath.Join(dir, "moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(link, shard); err != nil {
			t.Fatal(err)
		}
		return false
	})
	if err == nil || freed != 0 || removed != 0 {
		t.Errorf("trim = %d, %d, %v; want no removals and a confinement error", freed, removed, err)
	}
	if got, err := os.ReadFile(other); err != nil || string(got) != "outside data" { //nolint:gosec // reads only this test's temporary fixture
		t.Fatalf("trim changed a file outside the scanned cache: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "moved", filepath.Base(entryA))); err != nil {
		t.Errorf("original cached entry changed: %v", err)
	}
}

func trimLink(t *testing.T, target string) string {
	t.Helper()
	link := filepath.Join(t.TempDir(), "replacement")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	return link
}

func TestTrimKeepsReplacedRoot(t *testing.T) {
	const beforeTrim = "before trim"
	for _, when := range []string{beforeTrim, "during trim"} {
		t.Run(when, func(t *testing.T) {
			dir := cache(t, map[string][2]int{entryA: {100, 24}})
			outside := cache(t, map[string][2]int{entryA: {10, 1}})
			link := trimLink(t, outside)
			moved := filepath.Join(t.TempDir(), "original")
			u, err := Scan(dir)
			if err != nil {
				t.Fatal(err)
			}
			replace := func() {
				if err := os.Rename(dir, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(link, dir); err != nil {
					t.Fatal(err)
				}
			}
			if when == beforeTrim {
				replace()
			}
			freed, removed, err := Trim(u, 0, func() bool {
				if when == "during trim" {
					replace()
				}
				return false
			})
			if when == beforeTrim {
				if err == nil || freed != 0 || removed != 0 {
					t.Errorf("trim = %d, %d, %v; want the replaced root refused", freed, removed, err)
				}
				if _, err := os.Stat(filepath.Join(moved, entryA)); err != nil {
					t.Errorf("original entry changed after a refused trim: %v", err)
				}
			} else {
				if err != nil || freed != 100 || removed != 1 {
					t.Errorf("trim = %d, %d, %v; want only the original cache trimmed", freed, removed, err)
				}
				if _, err := os.Stat(filepath.Join(moved, entryA)); !os.IsNotExist(err) {
					t.Errorf("original entry remains: %v", err)
				}
			}
			if info, err := os.Stat(filepath.Join(outside, entryA)); err != nil || info.Size() != 10 {
				t.Errorf("outside entry changed: %v", err)
			}
		})
	}
}

func TestTrimPartialFailureKeepsAccurateCounts(t *testing.T) {
	dir := cache(t, map[string][2]int{entryA: {100, 1}, entryB: {100, 2}, entryC: {100, 3}})
	outside := cache(t, map[string][2]int{entryB: {10, 1}})
	link := trimLink(t, filepath.Join(outside, filepath.Dir(entryB)))
	u, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	shard := filepath.Join(dir, filepath.Dir(entryB))
	if err := os.Rename(shard, filepath.Join(dir, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(link, shard); err != nil {
		t.Fatal(err)
	}
	freed, removed, err := Trim(u, 0, func() bool { return false })
	if err == nil || freed != 100 || removed != 1 {
		t.Fatalf("trim = %d, %d, %v; want 100, 1 and a confinement error", freed, removed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, entryC)); !os.IsNotExist(err) {
		t.Errorf("oldest entry remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, entryA)); err != nil {
		t.Errorf("trim continued after its error: %v", err)
	}
	if info, err := os.Stat(filepath.Join(outside, entryB)); err != nil || info.Size() != 10 {
		t.Errorf("outside entry changed: %v", err)
	}
}

func TestTrimMissingRootIsEmpty(t *testing.T) {
	dir := cache(t, map[string][2]int{entryA: {100, 1}})
	u, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, filepath.Join(t.TempDir(), "moved")); err != nil {
		t.Fatal(err)
	}
	freed, removed, err := Trim(u, 0, func() bool { return false })
	if err != nil || freed != 0 || removed != 0 {
		t.Errorf("trim = %d, %d, %v; want nothing removed from a vanished cache", freed, removed, err)
	}
}

func TestTrimStopsAfterBuildStarts(t *testing.T) {
	entries := make(map[string][2]int)
	for i := range busyEvery + 2 {
		entries[fmt.Sprintf("00/entry-%03d-d", i)] = [2]int{1, i + 1}
	}
	dir := cache(t, entries)
	u, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	freed, removed, err := Trim(u, 0, func() bool { checks++; return checks == 2 })
	if !errors.Is(err, ErrBusy) || freed != busyEvery || removed != busyEvery || checks != 2 {
		t.Fatalf("trim = %d, %d, %v after %d checks; want a partial trim stopped by the build", freed, removed, err, checks)
	}
	left, err := Scan(dir)
	if err != nil || left.Bytes != 2 || len(left.Entries) != 2 {
		t.Errorf("remaining cache = %+v, %v; want the two newest entries preserved", left, err)
	}
}

// The sccache caller supplies its own scan, with arbitrary nested paths.
func TestTrimCallerSuppliedUsage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "data")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	u := Usage{Dir: dir, Bytes: 7, Entries: []Entry{{Path: path, Size: 7}}}
	freed, removed, err := Trim(u, 0, func() bool { return false })
	if err != nil || freed != 7 || removed != 1 {
		t.Errorf("trim = %d, %d, %v; want the caller-supplied payload removed", freed, removed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("payload remains: %v", err)
	}
}
