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

func TestConsecutiveToolOnlyStepsQueuedWakeDeliveredNextTurnOnce(t *testing.T) {
	stateDir := t.TempDir()
	agentID := "agent-tool-only"
	inbox := InboxPath(stateDir, agentID)
	logPath := LogPath(stateDir, agentID)
	callsFile := filepath.Join(stateDir, "calls.log")
	step1File := filepath.Join(stateDir, "step1.ready")
	releaseFile := filepath.Join(stateDir, "step1.release")

	// Turn 1 emits two consecutive tool-only steps (command_execution item_1 and
	// item_2, with no agent_message or reasoning item). While Turn 1 is paused
	// between tool 1 and tool 2, a wake message is queued. Turn 1 finishes its
	// second tool-only step and completes; Run then delivers the wake message
	// once as Turn 2 via `codex exec resume`.
	fakeCodex := filepath.Join(stateDir, "fake-codex-tools.sh")
	script := `#!/usr/bin/env bash
set -euo pipefail
prompt="$(cat)"
printf 'ARGS:%s|PROMPT:%s\n' "$*" "$prompt" >> "` + callsFile + `"
if [[ "$*" == *" resume "* ]]; then
  echo '{"type":"turn.started"}'
  echo '{"type":"item.started","item":{"id":"item_3","type":"command_execution"}}'
  echo '{"type":"item.completed","item":{"id":"item_3","type":"command_execution","command":"git status"}}'
  echo '{"type":"turn.completed","usage":{"input_tokens":40,"cached_input_tokens":10,"output_tokens":15}}'
else
  echo '{"type":"thread.started","thread_id":"thread-tools-1"}'
  echo '{"type":"turn.started"}'
  echo '{"type":"item.started","item":{"id":"item_1","type":"command_execution"}}'
  echo '{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"ls"}}'
  touch "` + step1File + `"
  while [[ ! -f "` + releaseFile + `" ]]; do sleep 0.01; done
  echo '{"type":"item.started","item":{"id":"item_2","type":"command_execution"}}'
  echo '{"type":"item.completed","item":{"id":"item_2","type":"command_execution","command":"pwd"}}'
  echo '{"type":"turn.completed","usage":{"input_tokens":80,"cached_input_tokens":20,"output_tokens":25}}'
fi
`
	if err := os.WriteFile(fakeCodex, []byte(script), 0o700); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var stderr bytes.Buffer
	go func() {
		done <- Run(ctx, inbox, logPath, fakeCodex, testModel, &stderr)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		err := Send(inbox, "turn 1 prompt")
		if err == nil {
			break
		}
		if !errors.Is(err, ErrNotRunning) || time.Now().After(deadline) {
			t.Fatalf("Send turn 1: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Wait for tool 1 in Turn 1 to complete.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(step1File); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Queue a wake message while Turn 1 is mid-turn between two tool-only steps.
	if err := Send(inbox, "wake queued during tool-only turn"); err != nil {
		t.Fatalf("Send mid-turn wake: %v", err)
	}
	if err := os.WriteFile(releaseFile, []byte("go\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Wait for Turn 2 to accept and durably confirm the wake before
	// cancellation: merely starting the subprocess leaves it unconfirmed.
	var calls string
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(callsFile) //nolint:gosec // test file
		calls = string(b)
		if strings.Count(calls, "ARGS:") >= 2 {
			q, err := readQueue(inbox)
			if err == nil && q.Inflight == "" && len(q.Messages) == 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(calls), "\n")
	if len(lines) != 2 {
		t.Fatalf("calls = %q, want 2 lines (no duplicate delivery)", calls)
	}
	if !strings.HasSuffix(lines[1], "|PROMPT:wake queued during tool-only turn") {
		t.Errorf("turn 2 = %q, want wake queued during tool-only turn", lines[1])
	}
	msgs, err := Drain(inbox)
	if err != nil || len(msgs) != 0 {
		t.Errorf("Drain after Turn 2 = (%v, %v), want empty", msgs, err)
	}
}

func TestRunLogOpenErrorDoesNotHang(t *testing.T) {
	stateDir := t.TempDir()
	inbox := InboxPath(stateDir, "agent-bad-log")
	// Pass an existing directory as logPath so os.OpenFile fails immediately.
	badLogPath := t.TempDir()

	done := make(chan error, 1)
	go func() {
		var stderr bytes.Buffer
		done <- Run(context.Background(), inbox, badLogPath, "codex", testModel, &stderr)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run with directory logPath = nil, want error")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Run hung when opening logPath failed")
	}
}

func TestStopRunnerStopsActiveChildTree(t *testing.T) {
	stateDir := t.TempDir()
	agentID := "agent-stop-tree"
	inbox := InboxPath(stateDir, agentID)
	logPath := LogPath(stateDir, agentID)
	childPIDFile := filepath.Join(stateDir, "child.pid")

	// Fake codex spawns a child sleep and blocks waiting for it during an active turn.
	fakeCodex := filepath.Join(stateDir, "fake-codex-active.sh")
	script := `#!/usr/bin/env bash
set -euo pipefail
cat >/dev/null
echo '{"type":"thread.started","thread_id":"thread-stop-tree"}'
echo '{"type":"turn.started"}'
sleep 60 &
child=$!
echo "$child" > "` + childPIDFile + `"
wait "$child"
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

	deadline := time.Now().Add(3 * time.Second)
	for {
		err := Send(inbox, "start blocking turn")
		if err == nil {
			break
		}
		if !errors.Is(err, ErrNotRunning) || time.Now().After(deadline) {
			t.Fatalf("Send: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	var childPID int
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(childPIDFile) //nolint:gosec // test file
		if err == nil && len(bytes.TrimSpace(b)) > 0 {
			if err := json.Unmarshal(bytes.TrimSpace(b), &childPID); err == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID <= 0 {
		t.Fatal("child PID was not written")
	}
	defer func() {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
	}()

	// Cancel the runner while the turn and its child sleep are active.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run error on cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not exit within 3s after cancel")
	}

	// Verify the child sleep process is no longer alive.
	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child process %d remained alive after runner stopped (kill(0) = %v)", childPID, err)
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

	// Wait for the background sleep PID to be written and for the turn to finish.
	var bgPIDBytes []byte
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidFile) //nolint:gosec // test file
		if err == nil && len(bytes.TrimSpace(b)) > 0 && ReadState(inbox) == StateIdle {
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

	// Stop the idle runner while the background child is still alive.
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

func TestRemoveInboxRemovesAllCompanionFilesWhenFIFOMissing(t *testing.T) {
	stateDir := t.TempDir()
	const id = "agent-cleanup"
	const threadID = "thread-cleanup-1"
	inbox := InboxPath(stateDir, id)
	if err := MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	fifo, err := openInboxRDWR(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if err := Send(inbox, "queued msg"); err != nil {
		t.Fatal(err)
	}
	_ = fifo.Close()
	WriteState(inbox, StateIdle)
	if err := RecordThread(stateDir, inbox, threadID); err != nil {
		t.Fatal(err)
	}
	logPath := LogPath(stateDir, id)
	if err := os.WriteFile(logPath, []byte(`{"type":"thread.started"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Remove the .in FIFO first to simulate a partial cleanup where .in is gone.
	if err := os.Remove(inbox); err != nil {
		t.Fatal(err)
	}

	removed, err := RemoveInbox(inbox)
	if err != nil || !removed {
		t.Fatalf("RemoveInbox = (%v, %v), want (true, nil)", removed, err)
	}
	for _, p := range []string{
		inbox,
		QueuePath(inbox),
		LockPath(inbox),
		StatePath(inbox),
		logPath,
		ThreadPath(stateDir, id),
		ThreadLinkPath(stateDir, threadID),
	} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expected %s to be removed, stat err = %v", p, err)
		}
	}
}

func TestUnconfirmedTurnRetryAndConfirm(t *testing.T) {
	stateDir := t.TempDir()
	const id = "agent-inflight-retry"
	inbox := InboxPath(stateDir, id)
	logPath := LogPath(stateDir, id)
	if err := MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	fifo, err := openInboxRDWR(inbox)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fifo.Close() }()

	if err := Send(inbox, "retryable turn msg"); err != nil {
		t.Fatal(err)
	}
	msgs, err := Drain(inbox)
	if err != nil || len(msgs) != 1 || msgs[0] != "retryable turn msg" {
		t.Fatalf("Drain 1 = (%v, %v), want [retryable turn msg]", msgs, err)
	}

	// Simulate codex failing before emitting turn.started: runTurn returns an
	// error without calling Confirm(inbox).
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logFile.Close() }()

	fakeFail := filepath.Join(stateDir, "fake-fail.sh")
	if err := os.WriteFile(fakeFail, []byte("#!/usr/bin/env bash\nexit 1\n"), 0o700); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}
	var threadID string
	var stderr bytes.Buffer
	if err := runTurn(context.Background(), stateDir, inbox, logFile, fakeFail, testModel, &threadID, msgs[0], &stderr); err == nil {
		t.Fatal("expected runTurn to fail")
	}

	// Next Drain returns the unconfirmed Inflight message again without losing it.
	msgs, err = Drain(inbox)
	if err != nil || len(msgs) != 1 || msgs[0] != "retryable turn msg" {
		t.Fatalf("Drain after failed turn = (%v, %v), want [retryable turn msg]", msgs, err)
	}
	if err := Confirm(inbox); err != nil {
		t.Fatal(err)
	}
	msgs, err = Drain(inbox)
	if err != nil || len(msgs) != 0 {
		t.Fatalf("Drain after Confirm = (%v, %v), want empty", msgs, err)
	}
}
