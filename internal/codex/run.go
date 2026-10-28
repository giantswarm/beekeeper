package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ExecArgv is the command line for a codex session's first turn, reading the
// prompt from stdin.
func ExecArgv(bin, model string) []string {
	argv := []string{
		bin,
		"exec",
		"--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"--dangerously-bypass-hook-trust",
	}
	if strings.TrimSpace(model) != "" {
		argv = append(argv, "-m", strings.TrimSpace(model))
	}
	return append(argv, "-")
}

// ResumeArgv is the command line for a codex session's subsequent turns,
// resuming threadID and reading the prompt from stdin.
func ResumeArgv(bin, model, threadID string) []string {
	argv := []string{
		bin,
		"exec",
		"resume",
		"--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"--dangerously-bypass-hook-trust",
	}
	if strings.TrimSpace(model) != "" {
		argv = append(argv, "-m", strings.TrimSpace(model))
	}
	return append(argv, strings.TrimSpace(threadID), "-")
}

func stateDirFromInbox(inbox string) string {
	dir := filepath.Dir(inbox)
	if filepath.Base(dir) == "codex" {
		return filepath.Dir(dir)
	}
	return ""
}

// Run drives a Codex CLI session one turn at a time from its persistent FIFO
// inbox. The first message runs `codex exec --json ... -`, captures the
// emitted thread_id, and subsequent messages run `codex exec resume --json ... <thread_id> -`.
// While a turn is running, PreToolUse hooks can drain newly queued messages
// mid-turn; any message remaining in the queue when a turn ends is delivered
// immediately as the next turn.
func Run(ctx context.Context, inbox, logPath, bin, model string, stderr io.Writer) error {
	if err := MakeInbox(inbox); err != nil {
		return err
	}
	fifo, err := openInboxRDWR(inbox)
	if err != nil {
		return err
	}
	defer func() {
		WriteState(inbox, StateIdle)
		_ = fifo.Close()
	}()

	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // log path under state directory
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()

	stateDir := stateDirFromInbox(inbox)
	threadID := ReadThread(stateDir, inbox)
	WriteState(inbox, StateIdle)

	wake := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 256)
		for {
			n, rerr := fifo.Read(buf)
			if n > 0 {
				select {
				case wake <- struct{}{}:
				default:
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	for {
		if ctx.Err() != nil {
			return nil
		}
		msgs, err := Drain(inbox)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			WriteState(inbox, StateIdle)
			select {
			case <-ctx.Done():
				return nil
			case <-wake:
				continue
			}
		}
		WriteState(inbox, StateBusy)
		if err := runTurn(ctx, stateDir, inbox, logFile, bin, model, &threadID, msgs[0], stderr); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			_, _ = fmt.Fprintf(stderr, "codex-run: turn exited with error: %v\n", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
}

type streamEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
}

func runTurn(ctx context.Context, stateDir, inbox string, logFile *os.File, bin, model string, threadID *string, prompt string, stderr io.Writer) error {
	var argv []string
	if *threadID == "" {
		argv = ExecArgv(bin, model)
	} else {
		argv = ResumeArgv(bin, model, *threadID)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv is built from the configured codex binary and model
	if !strings.HasSuffix(prompt, "\n") {
		prompt += "\n"
	}
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	accepted := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		_, _ = logFile.Write(append(append([]byte(nil), line...), '\n'))
		var ev streamEvent
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.Type == evThreadStarted && ev.ThreadID != "" {
			*threadID = ev.ThreadID
			_ = RecordThread(stateDir, inbox, ev.ThreadID)
		}
		switch ev.Type {
		case evTurnStarted:
			accepted = true
			_ = Confirm(inbox)
		case "item.started", evItemCompleted, evTurnCompleted:
			accepted = true
			_ = Confirm(inbox)
			_ = ConfirmInjected(inbox)
		}
	}
	waitErr := cmd.Wait()
	if waitErr == nil && (accepted || *threadID != "") {
		_ = Confirm(inbox)
		_ = ConfirmInjected(inbox)
	}
	return waitErr
}
