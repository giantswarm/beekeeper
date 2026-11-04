package gocache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScanAndTrimExecutableEntries(t *testing.T) {
	old := filepath.Join("aa", strings.Repeat("a", 64)+"-d", "tool")
	warm := filepath.Join("bb", strings.Repeat("b", 64)+"-d", "tool")
	regular := "cc/object-d"
	dir := cache(t, map[string][2]int{old: {200, 1}, regular: {100, 24}, warm: {300, 96}})
	// Go updates the enclosing output directory on a cache hit, leaving
	// the executable's own timestamp unchanged.
	now := time.Now()
	for name, age := range map[string]time.Duration{old: 48 * time.Hour, warm: time.Hour} {
		used := now.Add(-age)
		if err := os.Chtimes(filepath.Dir(filepath.Join(dir, name)), used, used); err != nil {
			t.Fatal(err)
		}
	}
	u, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if u.Bytes != 600 || len(u.Entries) != 3 {
		t.Fatalf("scan = %d bytes in %d entries, want 600 in 3 including executable outputs", u.Bytes, len(u.Entries))
	}
	for i, name := range []string{old, regular, warm} {
		if got := u.Entries[i].Path; got != filepath.Join(dir, name) {
			t.Errorf("entry %d = %s, want %s ordered by Go's last-use timestamp", i, got, name)
		}
	}
	freed, removed, err := Trim(u, 300, func() bool { return false })
	if err != nil || freed != 300 || removed != 2 {
		t.Fatalf("trim = %d bytes, %d entries, %v; want 300, 2, nil", freed, removed, err)
	}
	for _, name := range []string{old, regular} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("old entry %s remains: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, warm)); err != nil {
		t.Fatalf("recently reused executable removed: %v", err)
	}
	left, err := Scan(dir)
	if err != nil || left.Bytes != 300 || len(left.Entries) != 1 {
		t.Fatalf("after trim = %+v, %v; want the 300-byte warm executable", left, err)
	}
}

func TestScanExecutableEntriesLeaveOtherData(t *testing.T) {
	output := filepath.Join("aa", strings.Repeat("a", 64)+"-d")
	names := map[string][2]int{
		filepath.Join(output, "tool"):                             {7, 1},
		filepath.Join(output, "nested", "data"):                   {1000, 1},
		"bb/ordinary/data":                                        {1000, 1},
		"cc/not-a-hash-d/data":                                    {1000, 1},
		filepath.Join("dd", strings.Repeat("d", 64)+"-a", "data"): {1000, 1},
	}
	dir := cache(t, names)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, output, "link")); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	u, err := Scan(dir)
	if err != nil || u.Bytes != 7 || len(u.Entries) != 1 {
		t.Fatalf("scan = %+v, %v; want only the 7-byte executable", u, err)
	}
	freed, removed, err := Trim(u, 0, func() bool { return false })
	if err != nil || freed != 7 || removed != 1 {
		t.Fatalf("trim = %d, %d, %v; want 7, 1, nil", freed, removed, err)
	}
	for name := range names {
		if name == filepath.Join(output, "tool") {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("unrelated file %s changed: %v", name, err)
		}
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "outside data" { //nolint:gosec // reads only this test's temporary fixture
		t.Errorf("outside data changed: %q, %v", got, err)
	}
}
