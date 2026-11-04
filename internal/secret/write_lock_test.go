package secret

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSOPSWriteHeldLock(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	file := filepath.Join(t.TempDir(), "destination.yaml")
	original := []byte("synthetic encrypted bytes")
	if err := os.WriteFile(file, original, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := readSnapshot(file)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := writeLock(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock() //nolint:errcheck // test cleanup
	if err := commitEncrypted(file, []byte("replacement"), snapshot); !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	raw, err := os.ReadFile(file) //nolint:gosec // a path of this test
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(original) {
		t.Fatal("held lock allowed a write")
	}
	entries, err := os.ReadDir(filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatal("lock polluted destination directory")
	}
}
