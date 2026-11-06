package gocache

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDirRejectsRelativeCacheBeforeTrimming(t *testing.T) {
	dir := cache(t, map[string][2]int{entryA: {100, 1}})
	t.Chdir(dir)
	t.Setenv("GOCACHE", ".")
	// Model the automatic trim caller: an empty discovery result disables
	// trimming. A nonempty result is scanned and may be trimmed over cap.
	if discovered := Dir(); discovered != "" {
		u, err := Scan(discovered)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := Trim(u, 0, func() bool { return false }); err != nil {
			t.Fatal(err)
		}
		t.Error("Dir accepted GOCACHE=., which Go rejects as non-absolute")
	}
	if _, err := os.Stat(filepath.Join(dir, entryA)); err != nil {
		t.Errorf("invalid cache discovery allowed a working-directory file to be removed: %v", err)
	}
}

func TestDirRejectsRelativeCachePaths(t *testing.T) {
	for _, path := range []string{".", "cache", filepath.Join("..", "cache")} {
		t.Run(path, func(t *testing.T) {
			t.Setenv("GOCACHE", path)
			if Dir() != "" {
				t.Error("Dir accepted a cache path that Go cannot use")
			}
		})
	}
}
