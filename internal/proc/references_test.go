package proc

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReferencesReadsOpenFilesMappingsAndBuildDirectories(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "42")
	if err := os.MkdirAll(filepath.Join(dir, "fd"), 0700); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(root, "work"), filepath.Join(root, "output"), filepath.Join(root, "mapped"), filepath.Join(root, "tmp")}
	if err := os.Symlink(paths[0], filepath.Join(dir, "cwd")); err != nil {
		t.Skip(err)
	}
	if err := os.Symlink(paths[1]+" (deleted)", filepath.Join(dir, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "maps"), []byte("00-10 r--p 0 0:0 1 "+paths[2]+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "environ"), []byte("GOTMPDIR="+paths[3]+"\x00UNRELATED=value\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := referencesAt(root, 42)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if !slices.Contains(got, p) {
			t.Errorf("missing reference %s", filepath.Base(p))
		}
	}
	// A file masquerading as /proc/<pid>/fd is not an empty set of files.
	if err := os.Remove(filepath.Join(dir, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "fd")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fd"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := referencesAt(root, 42); err == nil || strings.TrimSpace(err.Error()) == "" {
		t.Fatal("unreadable references looked unused")
	}
}
