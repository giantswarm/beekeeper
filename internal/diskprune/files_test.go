package diskprune

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestChildrenProtectsLiveRecentAndCheckouts(t *testing.T) {
	base := t.TempDir()
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	for _, name := range []string{"dead", "live", "recent", "checkout", "nested"} {
		d := filepath.Join(base, name)
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "output"), []byte("build output"), 0600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(filepath.Join(d, "output"), old, old)
		_ = os.Chtimes(d, old, old)
	}
	if err := os.WriteFile(filepath.Join(base, "checkout", checkoutMetadata), []byte("gitdir: missing"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(filepath.Join(base, "checkout"), old, old)
	_ = os.Chtimes(filepath.Join(base, "recent", "output"), now, now)
	_ = os.Chtimes(filepath.Join(base, "nested", "output"), now, now)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("work"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "link")); err != nil {
		t.Skip(err)
	}
	refs := []string{filepath.Join(base, "live", "output")}
	dry := Children(base, now.Add(-24*time.Hour), refs, true)
	if dry.Removed != 1 || dry.Freed == 0 {
		t.Fatalf("dry = %+v", dry)
	}
	if _, err := os.Stat(filepath.Join(base, "dead")); err != nil {
		t.Fatal("dry run deleted output")
	}
	r := Children(base, now.Add(-24*time.Hour), refs, false)
	if r.Removed != 1 || len(r.Errors) != 0 {
		t.Fatalf("prune = %+v", r)
	}
	for _, name := range []string{"live", "recent", "checkout", "nested", "link"} {
		if _, err := os.Lstat(filepath.Join(base, name)); err != nil {
			t.Errorf("lost %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "dead")); !os.IsNotExist(err) {
		t.Fatal("dead output survived")
	}
}

func TestInUseResolvesAliases(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skip(err)
	}
	if !InUse(real, []string{alias}) {
		t.Fatal("an alias to a live directory looked unused")
	}
	if InUse(real, []string{base}) {
		t.Fatal("a parent process blocks unrelated children")
	}
}

func TestOutputRemovesReadOnlyBuildData(t *testing.T) {
	d := filepath.Join(t.TempDir(), "output")
	if err := os.MkdirAll(filepath.Join(d, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "sub", "binary"), []byte("output"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(d, "sub"), 0500); err != nil { //nolint:gosec // read-only build output is intentional
		t.Fatal(err)
	}
	r := Output(d, time.Time{}, nil, false)
	if r.Removed != 1 || len(r.Errors) != 0 {
		t.Fatalf("prune = %+v", r)
	}
}

func TestChildrenDoesNotFollowPlaceSymlink(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "output"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skip(err)
	}
	r := Children(alias, time.Time{}, nil, false)
	if r.Removed != 0 || len(r.Kept) != 1 {
		t.Fatalf("followed a place symlink: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(real, "output")); err != nil {
		t.Fatal(err)
	}
}
