//go:build unix

package omp

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSend(t *testing.T) {
	inbox := InboxPath(t.TempDir(), "1234abcd")
	if err := Send(inbox, "hello"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("send before the inbox exists: %v, want ErrNotRunning", err)
	}
	if err := MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	if err := MakeInbox(inbox); err != nil {
		t.Fatalf("making it again: %v", err)
	}
	if err := Send(inbox, "hello"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("send with no reader: %v, want ErrNotRunning", err)
	}
	// The agent's shell opens its inbox for reading and writing.
	r, err := os.OpenFile(inbox, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err := Send(inbox, "  fix the test\nthen report  "); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(r).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var c command
	if err := json.Unmarshal(line, &c); err != nil || c.Type != "steer" || c.Message != "fix the test\nthen report" {
		t.Fatalf("inbox line %q: %+v %v", line, c, err)
	}
}

func TestMakeInboxRefusesAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.in")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MakeInbox(path); err == nil {
		t.Fatal("a regular file taken for an inbox")
	}
}
