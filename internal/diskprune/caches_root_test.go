package diskprune

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	cacheRootTestPath  = "sub/data"
	cacheRootTestEntry = "entry"
	cacheRootTestEmpty = "empty directory"
)

func TestCacheKeepsRootReplacedDuringScan(t *testing.T) {
	for _, files := range []bool{false, true} {
		name := "entries"
		if files {
			name = cacheModeFiles
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			root, moved, outside := filepath.Join(base, "cache"), filepath.Join(base, "moved"), filepath.Join(base, "outside")
			cacheFile(t, root, cacheRootTestPath, 1000, 10*day)
			victim := cacheFile(t, outside, cacheRootTestPath, 1000, 20*day)
			p := Policy{Root: root, Files: files, OlderThan: 7 * day, Recheck: func() ([]string, error) {
				if err := os.Rename(root, moved); err != nil {
					return nil, err
				}
				if err := os.Symlink(outside, root); err != nil {
					t.Skip(err)
				}
				return nil, nil
			}}
			r := Cache(p, time.Now(), false, nil, nil, false)
			if r.Freed != 0 || r.Removed != 0 || r.Kept[whyRootChanged] != 1 || !exists(victim) || !exists(filepath.Join(moved, filepath.FromSlash(cacheRootTestPath))) {
				t.Fatalf("root replacement redirected prune: %+v, outside=%v original=%v", r, exists(victim), exists(filepath.Join(moved, filepath.FromSlash(cacheRootTestPath))))
			}
		})
	}
}

// Exercise the operations after their parent was opened: a later rename must
// not redirect reinspection, deletion or empty-directory cleanup.
func TestCacheRemovalStaysInOpenedRoot(t *testing.T) {
	for _, operation := range []string{cacheRootTestEntry, "file", cacheRootTestEmpty} {
		t.Run(operation, func(t *testing.T) {
			base := t.TempDir()
			dir, moved, outside := filepath.Join(base, "cache"), filepath.Join(base, "moved"), filepath.Join(base, "outside")
			original := cacheFile(t, dir, cacheRootTestPath, 1000, 10*day)
			victim := cacheFile(t, outside, cacheRootTestPath, 1000, 20*day)
			u := inspect(original)
			if operation == cacheRootTestEntry {
				u = inspect(filepath.Dir(original))
			}
			if operation == cacheRootTestEmpty {
				if err := os.Remove(original); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(victim); err != nil {
					t.Fatal(err)
				}
				victim = filepath.Dir(victim)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			if err := os.Rename(dir, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, dir); err != nil {
				t.Skip(err)
			}
			switch operation {
			case cacheRootTestEntry:
				err = removeEntry(root, u, Refs{})
			case "file":
				err = removeFile(root, dir, u)
			case cacheRootTestEmpty:
				removeEmptied(root, map[string]bool{filepath.Dir(original): true})
			}
			if err != nil || !exists(victim) || exists(filepath.Join(moved, filepath.FromSlash(cacheRootTestPath))) {
				t.Fatalf("cleanup escaped opened root: err=%v outside=%v original=%v", err, exists(victim), exists(filepath.Join(moved, filepath.FromSlash(cacheRootTestPath))))
			}
			if operation == cacheRootTestEmpty && exists(filepath.Join(moved, "sub")) {
				t.Fatal("original empty directory survived")
			}
		})
	}
}

func TestMeasurePathForms(t *testing.T) {
	root := t.TempDir()
	file := cacheFile(t, root, cacheRootTestPath, 1000, 10*day)
	for _, path := range []string{root, root + string(filepath.Separator), file} {
		size, newest := Measure(path)
		if size != 1000 || newest.IsZero() {
			t.Errorf("Measure(%q) = %d, %v", path, size, newest)
		}
	}
	for _, path := range []string{"", filepath.Join(root, "missing")} {
		if size, newest := Measure(path); size != 0 || !newest.IsZero() {
			t.Errorf("missing Measure(%q) = %d, %v", path, size, newest)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Dir(file), link); err != nil {
		t.Skip(err)
	}
	if size, newest := Measure(link); size != 0 || !newest.IsZero() {
		t.Errorf("link Measure = %d, %v", size, newest)
	}
}

func TestCacheFilesKeepsMetadataRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), checkoutMetadata)
	data := cacheFile(t, root, "HEAD", 1000, 10*day)
	r := Cache(Policy{Root: root, Files: true, OlderThan: 7 * day}, time.Now(), false, nil, nil, false)
	if r.Removed != 0 || r.Kept[whyCheckout] != 1 || !exists(data) {
		t.Fatalf("metadata root lost its checkout protection: %+v", r)
	}
}
