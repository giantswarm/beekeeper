//go:build darwin

package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunningBinaryDetectsReplacement(t *testing.T) {
	b := RunningBinary()
	if b == nil || b.Path == "" {
		t.Fatal("running binary unavailable: the loop cannot restart after install")
	}
	if b.Replaced() {
		t.Fatal("unreplaced binary marked replaced")
	}
	// Exercise the same retained file identity against an isolated path.
	path := filepath.Join(t.TempDir(), "beekeeper")
	if err := os.WriteFile(path, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	b = &Binary{Path: path, self: fi}
	if b.Replaced() {
		t.Fatal("unchanged binary marked replaced")
	}
	replacement := path + ".new"
	if err := os.WriteFile(replacement, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if !b.Replaced() {
		t.Fatal("replacement not detected")
	}
	if err := b.Exec(); err == nil {
		t.Fatal("invalid executable unexpectedly ran")
	}
	if b.Replaced() {
		t.Fatal("failed exec retried without another replacement")
	}
}
