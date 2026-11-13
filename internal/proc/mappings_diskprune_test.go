package proc_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/diskprune"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// TestUserScanPreservesMappedFilenames runs outside package proc: diskprune
// imports it through worktrees and github.
//
// control: none: moved from package proc unchanged but for the package
// prefix, since diskprune now imports proc and an internal test of proc
// cannot import diskprune
func TestUserScanPreservesMappedFilenames(t *testing.T) {
	for _, name := range []string{"mapped ", "mapped\t", "mapped\r", "mapped ", "mapped (deleted)"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			root := proc.MappingFixture(t, name)
			candidate := filepath.Join(root, name)
			if err := os.WriteFile(candidate, []byte("mapped data"), 0o600); err != nil {
				t.Fatal(err)
			}
			scan, err := proc.UserScanAt(root, os.Getuid(), proc.ReferenceFiles())
			if err != nil {
				t.Fatal(err)
			}
			// A mapping may outlive its file descriptor. It must still keep
			// the actual candidate, including meaningful filename bytes.
			result := diskprune.Output(candidate, time.Time{}, scan.Refs, true)
			if result.Removed != 0 || result.Kept["in use"] != 1 {
				t.Fatalf("mapped candidate would be pruned: %+v; refs=%q", result, scan.Refs)
			}
			if name == "mapped (deleted)" && !diskprune.InUse(filepath.Join(root, "mapped"), scan.Refs) {
				t.Fatal("lost the original path of a deleted mapping")
			}
		})
	}
}

// TestReferencesPreserveLiteralDeletedSuffix runs outside package proc for
// the same reason.
//
// control: none: moved from package proc unchanged but for the package
// prefix, since diskprune now imports proc and an internal test of proc
// cannot import diskprune
func TestReferencesPreserveLiteralDeletedSuffix(t *testing.T) {
	root := proc.MappingFixture(t, "unused")
	if err := os.WriteFile(filepath.Join(root, "42", proc.FixtureMappings), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(root, "open (deleted)")
	if err := os.WriteFile(candidate, []byte("open data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(candidate, filepath.Join(root, "42", "fd", "3")); err != nil {
		t.Skip(err)
	}
	refs, err := proc.ReferencesAt(root, 42)
	if err != nil {
		t.Fatal(err)
	}
	for name, paths := range map[string][]string{"References": refs, "Held": proc.HeldAt(root, 42)} {
		result := diskprune.Output(candidate, time.Time{}, paths, true)
		if result.Removed != 0 || result.Kept["in use"] != 1 {
			t.Errorf("%s lost a live filename ending in the deletion marker", name)
		}
	}
}
