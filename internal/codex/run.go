package codex

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// IdleGrace is how long an idle runner waits after its session parks, finishes
// or leaves the roster before closing its inbox FIFO and exiting.
var IdleGrace = 200 * time.Millisecond

// ExecArgv is the command line for a codex session's first turn, reading the
// prompt from stdin.
func ExecArgv(bin, model string, effort ...string) []string {
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
	return append(withEffort(argv, effort), "-")
}

// ResumeArgv is the command line for a codex session's subsequent turns,
// resuming threadID and reading the prompt from stdin.
func ResumeArgv(bin, model, threadID string, effort ...string) []string {
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
	return append(withEffort(argv, effort), strings.TrimSpace(threadID), "-")
}

func withEffort(argv, effort []string) []string {
	if len(effort) > 0 && effort[0] != "" {
		return append(argv, "-c", fmt.Sprintf("model_reasoning_effort=%q", effort[0]))
	}
	return argv
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
// emitted thread_id, and subsequent messages run
// `codex exec resume --json ... <thread_id> -`. Messages arriving while a turn
// is running can be consumed by the PreToolUse hook as additional context.
// Messages still queued when the active turn finishes run as the next turn.
func Run(ctx context.Context, inbox, logPath, bin, model string, stderr io.Writer, effort ...string) error {
	ctx, stopSig := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSig()

	if err := MakeInbox(inbox); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // log path under state directory
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()

	fifo, err := openInboxRDWR(inbox)
	if err != nil {
		return err
	}
	stateDir := stateDirFromInbox(inbox)
	threadID := ReadThread(stateDir, inbox)
	id := cmp.Or(strings.TrimSuffix(filepath.Base(inbox), ".in"), strings.TrimSpace(os.Getenv(EnvAgent)))
	var hostSession string
	if id != "" {
		hostSession = HostPrefix + id
	}
	name := strings.TrimSpace(os.Getenv(EnvName))
	WriteState(inbox, StateIdle)

	readerDone := make(chan struct{})
	wake := make(chan struct{}, 1)
	go func() {
		defer close(readerDone)
		buf := make([]byte, 256)
		for {
			if ctx.Err() != nil {
				return
			}
			n, rerr := fifo.Read(buf)
			if ctx.Err() != nil {
				return
			}
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
	defer func() {
		WriteState(inbox, StateIdle)
		_, _ = fifo.Write([]byte{'\n'})
		_ = fifo.Close()
		<-readerDone
	}()

	idleSince := time.Now()
	var inactiveSince time.Time
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
			var poll <-chan time.Time
			if stateDir != "" {
				currentThread := cmp.Or(threadID, ReadThread(stateDir, inbox))
				if state.RunnerSessionActive(stateDir, hostSession, currentThread, name) {
					inactiveSince = time.Time{}
					poll = time.After(max(IdleGrace, 100*time.Millisecond))
				} else {
					if inactiveSince.IsZero() {
						inactiveSince = time.Now()
					}
					if time.Since(inactiveSince) >= IdleGrace && time.Since(idleSince) >= IdleGrace {
						closed, err := closeIfIdle(inbox, fifo)
						if err != nil {
							return err
						}
						if closed {
							return nil
						}
						continue
					}
					poll = time.After(max(IdleGrace/2, 10*time.Millisecond))
				}
			}
			select {
			case <-ctx.Done():
				return nil
			case <-wake:
				continue
			case <-poll:
				continue
			}
		}
		WriteState(inbox, StateBusy)
		if err := runTurn(ctx, stateDir, inbox, logFile, bin, model, &threadID, msgs[0], stderr, effort...); err != nil {
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
		idleSince = time.Now()
		inactiveSince = time.Time{}
	}
}

type streamEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
}

func runTurn(ctx context.Context, stateDir, inbox string, logFile *os.File, bin, model string, threadID *string, prompt string, stderr io.Writer, effort ...string) error {
	var argv []string
	if *threadID == "" {
		argv = ExecArgv(bin, model, effort...)
	} else {
		argv = ResumeArgv(bin, model, *threadID, effort...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv is built from the configured codex binary and model
	configureTurnCmd(cmd)
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
	defer func() {
		if ctx.Err() != nil {
			stopTurnProcessGroup(cmd)
		}
	}()

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
		case evTurnStarted, "item.started", evItemCompleted, evTurnCompleted:
			accepted = true
			_ = Confirm(inbox)
		}
	}
	scanErr := sc.Err()
	if scanErr != nil {
		// The child may be blocked writing to a pipe we can no longer read.
		// Use the configured cancellation path before waiting so Unix also
		// stops the turn's process group.
		_ = cmd.Cancel()
	}
	waitErr := cmd.Wait()
	if scanErr != nil {
		return fmt.Errorf("reading codex turn output: %w", scanErr)
	}
	if waitErr == nil && (accepted || *threadID != "") {
		_ = Confirm(inbox)
	}
	return waitErr
}
