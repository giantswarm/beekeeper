package gocache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestScanReportsUnreadableEntry(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires enforced Unix directory permissions")
	}
	dir := cache(t, map[string][2]int{entryA: {100, 1}})
	shard := filepath.Join(dir, filepath.Dir(entryA))
	t.Cleanup(func() { _ = os.Chmod(shard, 0o750) })
	// Directory names remain readable, but looking up their metadata fails.
	if err := os.Chmod(shard, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(dir); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("scan error = %v, want permission denied instead of a successful empty scan", err)
	}
}

func TestScanReturnsPartialUsageOnMetadataFailure(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires enforced Unix directory permissions")
	}
	dir := cache(t, map[string][2]int{"00/readable": {100, 1}, "ff/unreadable": {200, 1}})
	shard := filepath.Join(dir, "ff")
	t.Cleanup(func() { _ = os.Chmod(shard, 0o750) })
	if err := os.Chmod(shard, 0o400); err != nil {
		t.Fatal(err)
	}
	u, err := Scan(dir)
	if !errors.Is(err, fs.ErrPermission) || u.Bytes != 100 || len(u.Entries) != 1 {
		t.Fatalf("scan = %+v, %v; want the readable entry and a permission error", u, err)
	}
}
