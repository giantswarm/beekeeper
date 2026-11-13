package proc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The tests that check a scan against diskprune's decision are in
// mappings_diskprune_test.go, outside this package: diskprune imports it.

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
