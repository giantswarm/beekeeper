//go:build unix

package codex

import (
	"bytes"
	"os"
	"testing"
)

func TestSendPreservesUnreadableQueue(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode-000 files")
	}
	inbox := InboxPath(t.TempDir(), "unreadable-queue")
	if err := MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	fifo, err := openInboxRDWR(inbox)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fifo.Close() }()
	if err := Send(inbox, "already queued"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(QueuePath(inbox))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(QueuePath(inbox), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(QueuePath(inbox), 0o600) })
	sendErr := Send(inbox, "new wake")
	if err := os.Chmod(QueuePath(inbox), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(QueuePath(inbox))
	if err != nil {
		t.Fatal(err)
	}
	if sendErr == nil || !bytes.Equal(before, after) {
		t.Fatalf("Send must report the read failure and preserve the old queue: err=%v, preserved=%v", sendErr, bytes.Equal(before, after))
	}
}

func TestReadQueueReportsReadError(t *testing.T) {
	inbox := InboxPath(t.TempDir(), "queue-is-directory")
	if err := os.MkdirAll(QueuePath(inbox), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readQueue(inbox); err == nil {
		t.Fatal("queue read error was treated as an empty queue")
	}
}
