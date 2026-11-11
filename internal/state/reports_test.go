package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPendingReportsRejectsSymlinkOutsideInbox(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	message, err := QueueReport(outside, Party{Name: "fixture sender"}, "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(store.Dir(), reportsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := message.ID + ".json"
	if err := os.Symlink(filepath.Join(outside.Dir(), reportsDir, name), filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	// The target is a valid report with a matching filename. Only confinement
	// to the inbox should reject it, not JSON decoding or ID validation.
	if reports, err := PendingReports(store); err == nil || len(reports) != 0 {
		t.Fatalf("accepted report outside inbox: reports=%v, err=%v", reports, err)
	}
	if reports, err := PendingReports(outside); err != nil || len(reports) != 1 || reports[0].ID != message.ID {
		t.Fatalf("original report unreadable: reports=%v, err=%v", reports, err)
	}
}
