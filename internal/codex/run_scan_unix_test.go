//go:build unix

package codex

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunTurnOversizedOutput(t *testing.T) {
	const prompt = "retry this prompt"
	stateDir := t.TempDir()
	inbox := InboxPath(stateDir, "oversized-output")
	if err := writeQueue(inbox, persistentQueue{Inflight: prompt, InflightID: "pending"}); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(stateDir, "large-output.sh")
	// A large JSON text item exceeds the scanner ceiling and fills stdout's
	// pipe after the reader stops. No live Codex or external service is used.
	script := "#!/bin/sh\ncat >/dev/null\nexec cat \"$0.output\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil { //nolint:gosec // executable offline fixture under the test's temporary directory
		t.Fatal(err)
	}
	largeItem := `{"type":"item.completed","item":{"type":"agent_message","text":"` + strings.Repeat("x", maxLine+1024*1024) + "\"}}\n"
	if err := os.WriteFile(fake+".output", []byte(largeItem), 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := os.CreateTemp(stateDir, "stream-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var thread string
	err = runTurn(ctx, stateDir, inbox, log, fake, testModel, &thread, prompt, io.Discard)
	if ctx.Err() != nil {
		t.Fatal("runTurn blocked on the unread stdout pipe until cancellation")
	}
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("runTurn = %v, want scanner line-too-long error", err)
	}
	q, err := readQueue(inbox)
	if err != nil || q.Inflight != prompt {
		t.Fatalf("unaccepted prompt lost: %+v, %v", q, err)
	}
}
