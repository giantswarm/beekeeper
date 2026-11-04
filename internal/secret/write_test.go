package secret

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
)

func TestSOPSWriteRefusesContendedLock(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LOCALAPPDATA", cache)
	t.Setenv("HOME", cache)
	file := filepath.Join(t.TempDir(), "target.sops.yaml")
	original := []byte("synthetic-ciphertext")
	if err := os.WriteFile(file, original, 0o600); err != nil {
		t.Fatal(err)
	}
	doc := newDocument()
	if err := doc.remember(file, original, true); err != nil {
		t.Fatal(err)
	}
	path, err := sopsWriteLockPath(file)
	if err != nil {
		t.Fatal(err)
	}
	lock := flock.New(path)
	defer lock.Close() //nolint:errcheck // synthetic lock
	held, err := lock.TryLock()
	if err != nil || !held {
		t.Fatalf("fixture lock: %v", err)
	}
	if err := doc.commit(context.Background(), file, []byte("replacement")); !errors.Is(err, ErrSOPSConflict) {
		t.Fatalf("contended writer: %v", err)
	}
	after, err := os.ReadFile(file) //nolint:gosec // synthetic fixture
	if err != nil || string(after) != string(original) {
		t.Fatal("contended writer changed destination")
	}
	entries, err := os.ReadDir(filepath.Dir(file))
	if err != nil || len(entries) != 1 {
		t.Fatal("lock polluted destination directory")
	}
}
