package proc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/diskprune"
)

func TestUserScanPreservesMappedFilenames(t *testing.T) {
	for _, name := range []string{"mapped ", "mapped\t", "mapped\r", "mapped\u2003", "mapped (deleted)"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			root := mappingFixture(t, name)
			candidate := filepath.Join(root, name)
			if err := os.WriteFile(candidate, []byte("mapped data"), 0o600); err != nil {
				t.Fatal(err)
			}
			scan, err := userScanAt(root, os.Getuid(), referenceFiles())
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

func TestUserScanFailsClosedOnAmbiguousMappingNewlines(t *testing.T) {
	// Linux renders a newline as literal backslash-012 without escaping
	// backslashes. Decoding all escapes cannot resolve mixed literal and
	// escaped occurrences, so neither interpretation can establish safety.
	root := mappingFixture(t, `literal\012/escaped\012/mapped`)
	_, err := userScanAt(root, os.Getuid(), referenceFiles())
	var unknown *UnknownError
	if !errors.As(err, &unknown) || len(unknown.Processes) != 1 || unknown.Processes[0].PID != 42 {
		t.Fatalf("ambiguous mapping did not leave references unknown: %v", err)
	}
}

func TestReferencesPreserveLiteralDeletedSuffix(t *testing.T) {
	root := mappingFixture(t, "unused")
	if err := os.WriteFile(filepath.Join(root, "42", fixtureMappings), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(root, "open (deleted)")
	if err := os.WriteFile(candidate, []byte("open data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(candidate, filepath.Join(root, "42", "fd", "3")); err != nil {
		t.Skip(err)
	}
	refs, err := referencesAt(root, 42)
	if err != nil {
		t.Fatal(err)
	}
	for name, paths := range map[string][]string{"References": refs, "Held": held(root, 42)} {
		result := diskprune.Output(candidate, time.Time{}, paths, true)
		if result.Removed != 0 || result.Kept["in use"] != 1 {
			t.Errorf("%s lost a live filename ending in the deletion marker", name)
		}
	}
}

func mappingFixture(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "42")
	if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o700); err != nil {
		t.Fatal(err)
	}
	uid := os.Getuid()
	for file, data := range map[string]string{
		fixtureCommand:     fixtureName,
		fixtureStatus:      fmt.Sprintf("Uid: %d %d %d %d\n", uid, uid, uid, uid) + fixtureLiveStatus,
		fixtureEnvironment: "",
		fixtureMappings:    "00-10 r--p 0 0:0 1 " + filepath.Join(root, name) + "\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
