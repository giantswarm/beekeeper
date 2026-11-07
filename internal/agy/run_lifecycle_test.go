//go:build unix

package agy

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type agyReadyWriter struct {
	ready chan struct{}
	once  sync.Once
}

func (w *agyReadyWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "ready") {
		w.once.Do(func() { close(w.ready) })
	}
	return len(p), nil
}

func TestRunBoundsInheritedOutput(t *testing.T) {
	const modeExit, modeCancel = "exit", "cancel"
	for _, mode := range []string{modeExit, modeCancel} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			holdPath := filepath.Join(dir, "hold.in")
			if err := MakeInbox(holdPath); err != nil {
				t.Fatal(err)
			}
			hold, err := os.OpenFile(holdPath, os.O_RDWR, 0) //nolint:gosec // fixture FIFO
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ready := &agyReadyWriter{ready: make(chan struct{})}
			done := make(chan error, 1)
			finished := make(chan struct{})
			// A background tool retains stdout/stderr after agy exits or
			// is cancelled. Releasing the fixture FIFO always ends the tool.
			script := `(exec 3< "$1"; printf 'ready\n' >&2; read -r line <&3) &
if [ "$2" = cancel ]; then read -r line; fi
`
			go func() {
				defer close(finished)
				done <- Run(ctx, InboxPath(dir, "runner"), StreamPath(dir, "runner"),
					[]string{"sh", "-c", script, "agy-fixture", holdPath, mode}, ready)
			}()
			t.Cleanup(func() {
				_, _ = io.WriteString(hold, "release\n")
				cancel()
				_ = hold.Close()
				select {
				case <-finished:
				case <-time.After(drainWait + 2*time.Second):
					t.Error("Run did not finish during fixture cleanup")
				}
			})
			select {
			case <-ready.ready:
			case err := <-done:
				t.Fatalf("Run ended before descendant was ready: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("fixture descendant did not start")
			}
			if mode == modeCancel {
				cancel()
			}
			select {
			case err := <-done:
				if mode == modeExit && err != nil {
					t.Fatalf("Run after successful child exit: %v", err)
				}
				if mode == modeCancel && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled Run = %v; want context.Canceled", err)
				}
			case <-time.After(drainWait + 2*time.Second):
				t.Error("Run exceeded its output drain bound while a descendant held stdout open")
				_, _ = io.WriteString(hold, "release\n")
				cancel()
				select {
				case exitErr := <-done:
					if exitErr != nil {
						t.Logf("fixture cleanup: %v", exitErr)
					}
				case <-time.After(2 * time.Second):
					t.Error("Run did not end after releasing fixture output")
				}
			}
		})
	}
}

func TestRunDrainsFinalAcknowledgements(t *testing.T) {
	dir := t.TempDir()
	inbox := InboxPath(dir, "final-ack")
	if err := MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	seed, err := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // synthetic FIFO
	if err != nil {
		t.Fatal(err)
	}
	if err := Send(inbox, "accepted before exit"); err != nil {
		_ = seed.Close()
		t.Fatal(err)
	}
	_ = seed.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	script := `printf '%s\n' '{"event":"init","conversation_id":"final-ack-conv"}'
read -r line
printf '%s\n' '{"event":"step_update","step_update":{"step_type":"user_input"}}' '{"event":"result"}'
`
	if err := Run(ctx, inbox, StreamPath(dir, "final-ack"), []string{"sh", "-c", script}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := restoreInflight(inbox); err != nil {
		t.Fatal(err)
	}
	if got, err := Drain(inbox); err != nil || len(got) != 0 {
		t.Fatalf("accepted message replayed after child exit: %q, %v", got, err)
	}
}
