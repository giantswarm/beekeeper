package teleport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestRenewBackupFailureLeavesHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix directory permissions")
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	r := renewal(t, fakeTsh{until: now.Add(12 * time.Hour)})
	if err := os.MkdirAll(r.Previous(), 0o700); err != nil {
		t.Fatal(err)
	}
	archived := filepath.Join(r.Previous(), "archived")
	if err := os.WriteFile(archived, []byte("older profile"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(r.Previous(), 0o500); err != nil { //nolint:gosec // directory needs traversal permission to exercise failed cleanup
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(r.Previous(), 0o700) }) //nolint:gosec // restore owner-only directory access for test cleanup
	// Check the actual permission boundary, since privileged runners can
	// remove entries even from a directory without write permission.
	if err := os.Remove(archived); err == nil {
		t.Skip("runner bypasses directory write permissions")
	} else if !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	if _, err := r.Renew(t.Context(), now); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("renewal error = %v, want a backup permission failure", err)
	}
	if got, err := os.ReadFile(filepath.Join(r.Home, "profile")); err != nil || string(got) != homeUntil {
		t.Fatalf("failed renewal changed the active profile: %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Clean(archived)); err != nil || string(got) != "older profile" {
		t.Fatalf("backup = %q, %v", got, err)
	}
	if left, err := filepath.Glob(filepath.Join(r.Dir, "staging-*")); err != nil || len(left) != 0 {
		t.Fatalf("staging left behind: %v, %v", left, err)
	}
}

func TestRenewBackupPreparationFailureLeavesHome(t *testing.T) {
	r := renewal(t, fakeTsh{})
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.swap(filepath.Join(r.Dir, "missing-staging")); err == nil {
		t.Fatal("missing staging did not fail")
	}
	if got, err := readProfile(context.Background(), r.Home); err != nil || got.ValidUntil.Format(time.RFC3339) != homeUntil {
		t.Fatalf("failed preparation changed Home: %+v, %v", got, err)
	}
}
