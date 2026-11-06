package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecoverySnapshotRetainsValidatedSettingsAndSource(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "config.yaml")
	snapshot := filepath.Join(dir, "fatal-recovery.yaml")
	if err := os.WriteFile(source, []byte("stateDir: "+dir+"\ncapacity: {ceiling: 2}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverySnapshot(source, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("capacity: ["), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverySnapshot(source, snapshot); err == nil {
		t.Fatal("corrupt source replaced snapshot")
	}
	c, err := LoadRecovery(source, snapshot)
	if err != nil || c.Capacity.Ceiling != 2 {
		t.Fatalf("settings lost: %v", err)
	}
	if _, err := LoadRecovery(source+"other", snapshot); err == nil {
		t.Fatal("accepted another source")
	}
	info, err := os.Stat(snapshot)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("snapshot permissions", err)
	}
	if err := os.WriteFile(snapshot, []byte(recoveryHeader(source)+"stateDir: /another-desk\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRecovery(source, snapshot); err == nil {
		t.Fatal("accepted another desk")
	}
}
