package diskprune

import (
	"os"
	"path/filepath"
	"syscall"
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

func TestStageMovesOutputWithoutFreeingAndStartsTrashRetention(t *testing.T) {
	base := t.TempDir()
	dir, trash := filepath.Join(base, "target"), filepath.Join(base, "trash")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "binary"), []byte("output"), 0400); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	dry := Stage(dir, trash, now, nil, true)
	if dry.Staged != 1 || dry.Freed != 0 || dry.Moved == 0 {
		t.Fatalf("dry stage=%+v", dry)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("preview moved output")
	}
	held := Stage(dir, trash, now, []string{filepath.Join(dir, "sub", "binary")}, false)
	if held.Staged != 0 || len(held.Kept) == 0 {
		t.Fatal("moved active output")
	}
	staged := Stage(dir, trash, now, nil, false)
	if staged.Staged != 1 || staged.Freed != 0 || staged.Removed != 0 || len(staged.Errors) > 0 {
		t.Fatalf("stage=%+v", staged)
	}
	entries, err := os.ReadDir(trash)
	if err != nil || len(entries) != 1 {
		t.Fatalf("trash entries=%v err=%v", entries, err)
	}
	moved := filepath.Join(trash, entries[0].Name(), "target", "sub", "binary")
	if _, err := os.Stat(moved); err != nil {
		t.Fatal("output was deleted instead of moved")
	}
	recent := Children(trash, now.Add(-time.Hour), nil, false)
	if recent.Removed != 0 {
		t.Fatal("newly staged output bypassed retention")
	}
	active := Children(trash, time.Time{}, []string{moved}, false)
	if active.Removed != 0 {
		t.Fatal("pressure removed referenced trash")
	}
	expired := Children(trash, now.Add(time.Hour), nil, false)
	if expired.Removed != 1 || expired.Freed == 0 || len(expired.Errors) > 0 {
		t.Fatalf("expired trash=%+v", expired)
	}
}

func TestStageKeepsCrossFilesystemOutput(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "target")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	r := stage(dir, filepath.Join(base, "trash"), time.Now(), nil, false, func(string, string) error { return syscall.EXDEV })
	if r.Staged != 0 || r.Freed != 0 || len(r.Kept) == 0 {
		t.Fatalf("cross-filesystem stage=%+v", r)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("cross-filesystem fallback deleted source")
	}
}
