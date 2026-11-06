package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoverySnapshotRetainsLastValidAdmission(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := LoadRecovery(dir); err == nil {
		t.Fatal("missing snapshot accepted as defaults")
	}
	raw := fmt.Sprintf("stateDir: %q\ncapacity: {local: {ceiling: 0}}\nwatch: {interval: 13s}\n", dir)
	if err := os.WriteFile(source, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SaveRecovery(dir, source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("fatal: ["), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SaveRecovery(dir, source); err == nil {
		t.Fatal("invalid config replaced trusted snapshot")
	}
	cfg, err := LoadRecovery(dir)
	if err != nil || cfg.Capacity.Local.Ceiling == nil || *cfg.Capacity.Local.Ceiling != 0 || cfg.Watch.Interval.String() != "13s" {
		t.Fatalf("last valid admission lost: %v", err)
	}
	if err := os.Chmod(RecoveryPath(dir), 0644); err != nil { //nolint:gosec // exercise rejection of an exposed recovery file
		t.Fatal(err)
	}
	if _, err := LoadRecovery(dir); err == nil {
		t.Fatal("non-private snapshot trusted")
	}
	if err := SaveRecovery(t.TempDir(), source); err == nil {
		t.Fatal("invalid config trusted")
	}
}

func TestRecoverySnapshotCannotChangePinnedStore(t *testing.T) {
	dir := t.TempDir()
	path := RecoveryPath(dir)
	if err := os.WriteFile(path, []byte(fmt.Sprintf("stateDir: %q\n", t.TempDir())), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRecovery(dir); err == nil {
		t.Fatal("snapshot redirected pinned store")
	}
	if err := SaveRecovery(dir, path); err == nil {
		t.Fatal("snapshot with another store admitted")
	}
}

func TestRecoverySnapshotBindsOriginalSource(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(source, []byte(fmt.Sprintf("stateDir: %q\n", dir)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SaveRecovery(dir, source); err != nil {
		t.Fatal(err)
	}
	if got, err := RecoverySource(dir); err != nil || got != source {
		t.Fatalf("original source lost: %q %v", got, err)
	}
	if _, err := LoadRecoveryForSource(dir, source+"other"); err == nil {
		t.Fatal("snapshot admitted another source")
	}
}
