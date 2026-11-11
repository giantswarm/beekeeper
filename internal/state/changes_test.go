package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestChangesObserveAtomicReplacementAndCancellation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	changes, closeChanges := Changes(ctx, path, filepath.Join(dir, "missing", "fixture"))
	defer closeChanges()
	if changes == nil {
		t.Fatal("no filesystem subscription")
	}
	for i := range 2 {
		if err := writeJSON(path, &State{NextNote: i}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-changes:
		case <-time.After(5 * time.Second):
			t.Fatal("atomic rename did not wake the reader")
		}
	}
	cancel()
}

func TestChangesObserveLeaseDirectoryRemoval(t *testing.T) {
	dir := t.TempDir()
	lease := filepath.Join(dir, "fixture")
	if err := os.Mkdir(lease, 0o700); err != nil {
		t.Fatal(err)
	}
	changes, closeChanges := Changes(t.Context(), lease)
	defer closeChanges()
	if err := os.Rename(lease, filepath.Join(dir, "released")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	case <-time.After(5 * time.Second):
		t.Fatal("lease release did not wake the waiter")
	}
}
