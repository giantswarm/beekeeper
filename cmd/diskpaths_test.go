package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
