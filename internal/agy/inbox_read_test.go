//go:build unix

package agy

import (
	"errors"
	"os"
	"testing"
)

func TestSendPreservesUnreadableQueue(t *testing.T) {
	inbox := InboxPath(t.TempDir(), "unreadable")
	if err := MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	reader, err := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // synthetic FIFO
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if err := Send(inbox, "already stored"); err != nil {
		t.Fatal(err)
	}
	qp := queuePath(inbox)
	before, err := os.ReadFile(qp) //nolint:gosec // synthetic queue
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(qp, 0o200); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(qp, 0o600) })
	if _, err := os.ReadFile(qp); !errors.Is(err, os.ErrPermission) { //nolint:gosec // synthetic queue
		t.Skip("requires an unprivileged user for a denied queue read")
	}
	sendErr := Send(inbox, "new wake")
	if err := os.Chmod(qp, 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(qp) //nolint:gosec // synthetic queue
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(sendErr, os.ErrPermission) || string(after) != string(before) {
		t.Fatalf("Send error = %v; queue preserved = %v", sendErr, string(after) == string(before))
	}
}

func TestReadQueueRejectsReadErrors(t *testing.T) {
	inbox := InboxPath(t.TempDir(), "directory-queue")
	if err := os.MkdirAll(queuePath(inbox), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readQueue(inbox); err == nil {
		t.Fatal("reading a directory as a queue succeeded; want the read error")
	}
}
