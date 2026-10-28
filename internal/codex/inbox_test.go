//go:build unix

package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestInboxSendNotRunning(t *testing.T) {
	stateDir := t.TempDir()
	inbox := InboxPath(stateDir, "agent-1")
	if err := MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	if err := Send(inbox, "hello"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Send without runner = %v, want ErrNotRunning", err)
	}
	removed, err := RemoveInbox(inbox)
	if err != nil || !removed {
		t.Fatalf("RemoveInbox = (%v, %v), want (true, nil)", removed, err)
	}
}

func TestRunTurnAtATimeAndResume(t *testing.T) {
	stateDir := t.TempDir()
	agentID := "agent-turns"
	inbox := InboxPath(stateDir, agentID)
	logPath := LogPath(stateDir, agentID)
	callsFile := filepath.Join(stateDir, "calls.log")

	fakeCodex := filepath.Join(stateDir, "fake-codex.sh")
	script := `#!/usr/bin/env bash
set -euo pipefail
prompt="$(cat)"
printf 'ARGS:%s|PROMPT:%s\n' "$*" "$prompt" >> "` + callsFile + `"
if [[ "$*" == *" resume "* ]]; then
  echo '{"type":"turn.started"}'
  echo '{"type":"item.completed","item":{"type":"agent_message","text":"resumed reply"}}'
  echo '{"type":"turn.completed","usage":{"input_tokens":50,"cached_input_tokens":10,"output_tokens":20}}'
else
  echo '{"type":"thread.started","thread_id":"thread-abc-123"}'
  echo '{"type":"turn.started"}'
  echo '{"type":"item.completed","item":{"type":"agent_message","text":"first reply"}}'
  echo '{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":20,"output_tokens":30}}'
fi
`
	if err := os.WriteFile(fakeCodex, []byte(script), 0o700); err != nil { //nolint:gosec // test executable script
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var stderr bytes.Buffer
	go func() {
		done <- Run(ctx, inbox, logPath, fakeCodex, testModel, &stderr)
	}()

	// Wait until runner holds the FIFO open.
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := Send(inbox, "first turn prompt")
		if err == nil {
			break
		}
		if !errors.Is(err, ErrNotRunning) || time.Now().After(deadline) {
			t.Fatalf("Send first turn: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := Send(inbox, "second turn prompt"); err != nil {
		t.Fatalf("Send second turn: %v", err)
	}

	// Wait until both turns are recorded in callsFile.
	var calls string
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(callsFile) //nolint:gosec // test file
		calls = string(b)
		if strings.Count(calls, "ARGS:") >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run error: %v (stderr: %s)", err, stderr.String())
	}

	lines := strings.Split(strings.TrimSpace(calls), "\n")
	if len(lines) != 2 {
		t.Fatalf("calls = %q, want 2 lines", calls)
	}
	wantFirst := "ARGS:exec --json --dangerously-bypass-approvals-and-sandbox --dangerously-bypass-hook-trust -m gpt-5.7 -|PROMPT:first turn prompt"
	if lines[0] != wantFirst {
		t.Errorf("turn 1 = %q, want %q", lines[0], wantFirst)
	}
	wantSecond := "ARGS:exec resume --json --dangerously-bypass-approvals-and-sandbox --dangerously-bypass-hook-trust -m gpt-5.7 thread-abc-123 -|PROMPT:second turn prompt"
	if lines[1] != wantSecond {
		t.Errorf("turn 2 = %q, want %q", lines[1], wantSecond)
	}
	if got := ReadThread(stateDir, inbox); got != "thread-abc-123" {
		t.Errorf("ReadThread = %q, want thread-abc-123", got)
	}
}

func TestPreToolUseMidTurnWakeAndDedup(t *testing.T) {
	stateDir := t.TempDir()
	agentID := "agent-midturn"
	inbox := InboxPath(stateDir, agentID)
	if err := MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	fifo, err := openInboxRDWR(inbox)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fifo.Close() }()

	if err := RecordThread(stateDir, inbox, "thread-mid-1"); err != nil {
		t.Fatal(err)
	}
	if err := Send(inbox, "wake message 1"); err != nil {
		t.Fatal(err)
	}
	if err := Send(inbox, "wake message 2"); err != nil {
		t.Fatal(err)
	}

	hookStdin := []byte(`{"session_id":"thread-mid-1","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}`)
	out := PreToolUse(stateDir, hookStdin)
	if len(out) == 0 {
		t.Fatalf("expected PreToolUse output for queued wake messages")
	}
	var resp preToolUseHookResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal PreToolUse output: %v", err)
	}
	if resp.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Errorf("HookEventName = %q, want PreToolUse", resp.HookSpecificOutput.HookEventName)
	}
	if resp.HookSpecificOutput.AdditionalContext != "wake message 1\n\nwake message 2" {
		t.Errorf("AdditionalContext = %q", resp.HookSpecificOutput.AdditionalContext)
	}

	// If the first hook timed out before tool execution started, a retry before
	// ConfirmInjected re-delivers the same messages in FIFO order.
	retryOut := PreToolUse(stateDir, hookStdin)
	if !bytes.Equal(retryOut, out) {
		t.Errorf("retry PreToolUse before confirm = %s, want %s", string(retryOut), string(out))
	}

	// Once the runner sees the tool call start/complete and calls ConfirmInjected,
	// subsequent PreToolUse calls and subsequent turns do not re-deliver them.
	if err := ConfirmInjected(inbox); err != nil {
		t.Fatal(err)
	}
	if afterConfirm := PreToolUse(stateDir, hookStdin); len(afterConfirm) != 0 {
		t.Errorf("expected empty PreToolUse after ConfirmInjected, got %s", string(afterConfirm))
	}
	msgs, err := Drain(inbox)
	if err != nil || len(msgs) != 0 {
		t.Errorf("Drain after ConfirmInjected = (%v, %v), want empty", msgs, err)
	}
}

func TestChildProcessDoesNotInheritFIFO(t *testing.T) {
	stateDir := t.TempDir()
	agentID := "agent-cloexec"
	inbox := InboxPath(stateDir, agentID)
	logPath := LogPath(stateDir, agentID)
	pidFile := filepath.Join(stateDir, "bg.pid")

	// Fake codex spawns a background process that stays alive after fake-codex
	// and codex-run exit. Because openInboxRDWR sets O_CLOEXEC, the background
	// process must not hold the FIFO open.
	fakeCodex := filepath.Join(stateDir, "fake-codex-bg.sh")
	script := `#!/usr/bin/env bash
set -euo pipefail
cat >/dev/null
sleep 30 >/dev/null 2>&1 &
echo $! > "` + pidFile + `"
echo '{"type":"thread.started","thread_id":"thread-bg"}'
echo '{"type":"turn.started"}'
echo '{"type":"turn.completed"}'
`
	if err := os.WriteFile(fakeCodex, []byte(script), 0o700); err != nil { //nolint:gosec // test executable script
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var stderr bytes.Buffer
	go func() {
		done <- Run(ctx, inbox, logPath, fakeCodex, testModel, &stderr)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		err := Send(inbox, "run background child")
		if err == nil {
			break
		}
		if !errors.Is(err, ErrNotRunning) || time.Now().After(deadline) {
			t.Fatalf("Send: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Wait for the background sleep PID to be written.
	var bgPIDBytes []byte
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidFile) //nolint:gosec // test file
		if err == nil && len(bytes.TrimSpace(b)) > 0 {
			bgPIDBytes = bytes.TrimSpace(b)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(bgPIDBytes) == 0 {
		cancel()
		t.Fatalf("background child pid was never written")
	}
	defer func() {
		var pid int
		if err := json.Unmarshal(bgPIDBytes, &pid); err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}()

	// Stop the runner while the background child is still alive.
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Send must fail with ErrNotRunning even though the background child spawned
	// by codex exec is still running.
	if err := Send(inbox, "wake after runner exit"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Send after runner exit with live child = %v, want ErrNotRunning", err)
	}
}
