package execution

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"

	"os/exec"
	"path/filepath"
	"strings"
)

// codexTurnFailure is the journal's reason for a turn that failed after its
// acceptance; the coordinator's inspect reports it with the failed phase.
const codexTurnFailure = "codex turn failed after acceptance; inspect agents logs and recover the execution"

// Codex thread events the runner reads from `codex exec --json`.
const (
	codexThreadStarted = "thread.started"
	codexTurnStarted   = "turn.started"
	codexTurnCompleted = "turn.completed"
	codexTurnFailed    = "turn.failed"
)

// codexArgs is one headless turn: the first creates the thread, every later
// one resumes the thread the journal recorded. The prompt is read from stdin.
// Approvals and codex's own sandbox are off as for local workers; the
// execution guard runs as the managed home's PreToolUse hook instead.
func codexArgs(model, workspace, thread string) []string {
	args := []string{"exec"}
	if thread != "" {
		args = append(args, "resume")
	}
	args = append(args, "--json", "--strict-config", "--dangerously-bypass-approvals-and-sandbox", "--dangerously-bypass-hook-trust", "-m", model)
	if thread == "" {
		args = append(args, "-C", workspace)
		return append(args, "-")
	}
	return append(args, thread, "-")
}

// runCodex owns a codex worker for its lifetime: the first turn carries the
// brief, each stored wake runs as the next resumed turn, and a finished report
// ends the loop once its turn is over. A wake is accepted when its turn has
// started on the recorded thread: the same receipt the Claude stream gives.
func (s *Server) runCodex(ctx context.Context, ref Ref, j Journal) error {
	h := s.Config.Harnesses[HarnessCodex]
	if err := s.linkCodexLogin(); err != nil {
		return errors.New("codex login link in the managed home is unavailable")
	}
	log, err := s.openLog(ref)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	var wake chan struct{}
	if j.Request.Messaging {
		listener, err := net.Listen("unix", s.messageSocket(ref))
		if err != nil {
			return errors.New("owned message runner already exists or its socket is unavailable")
		}
		defer func() { _ = listener.Close() }()
		wake = make(chan struct{}, 1)
		go func() {
			for {
				c, err := listener.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		}()
	}
	env := append(s.codexEnvironment(), EnvID+"="+string(ref.Execution), EnvCoordinator+"="+j.Request.Coordinator, "BEEKEEPER_CONFIG="+s.ConfigPath)
	first := func() error {
		if err := s.update(ctx, ref, func(j *Journal) error { j.Ready = true; return nil }); err != nil {
			return err
		}
		return nil
	}
	thread, accepted, turnErr := s.codexTurn(ctx, ref, h, env, log, j, j.Request.Prompt, "", first)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !accepted || thread == "" {
		return s.codexEnd(ctx, ref, Failed, "harness failed or ended before first-task acceptance")
	}
	// Acceptance is delivery evidence, not a finished turn: a turn that
	// fails afterwards ends the execution as failed, so its task is
	// recovered instead of a silent runner holding the lease and the room.
	if turnErr != nil {
		return s.codexEnd(ctx, ref, Failed, codexTurnFailure)
	}
	if !j.Request.Messaging {
		return s.codexEnd(ctx, ref, Exited, "")
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		current, err := s.Read(ref)
		if err != nil {
			return err
		}
		if current.Completed || current.StopRequest != nil {
			return s.codexEnd(ctx, ref, Exited, "")
		}
		next, ok := current.Messages.Next()
		if !ok {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-wake:
			}
			continue
		}
		// A queued wake cannot authorize a model call after the coordinator
		// has stopped it; the message stays stored for the next wake.
		if err := s.authorizeMessage(ctx, current); err != nil {
			if err := s.update(ctx, ref, func(j *Journal) error {
				j.LastError = "message remains stored: coordinating authority unavailable or stopped"
				return nil
			}); err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-wake:
			}
			continue
		}
		if err := s.update(ctx, ref, func(j *Journal) error {
			if j.StopRequest != nil {
				return errors.New("execution stopped before message delivery")
			}
			return j.Messages.Attempt(next)
		}); err != nil {
			return err
		}
		accept := func() error { return s.acceptCodexMessage(ctx, ref, current.Handle.ConversationID, next.ID) }
		_, accepted, turnErr := s.codexTurn(ctx, ref, h, env, log, current, codexMessageText(next), thread, accept)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if accepted && turnErr != nil {
			return s.codexEnd(ctx, ref, Failed, codexTurnFailure)
		}
		if !accepted {
			// The write's outcome is unknown: the receipt stays uncertain and
			// blocks replay until the coordinator resolves it.
			if err := s.update(ctx, ref, func(j *Journal) error {
				j.LastError = "wake turn ended before acceptance; resolve its uncertain receipt"
				return nil
			}); err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-wake:
			}
		}
	}
}

func codexMessageText(m Envelope) string {
	if m.Sender != (Sender{}) {
		return "Message from " + string(mustJSON(m.Sender)) + ":\n\n" + m.Text
	}
	return m.Text
}

// acceptCodexMessage commits ID-bearing evidence before the journal receipt,
// as the Claude stream does, so reconciliation survives a runner crash.
func (s *Server) acceptCodexMessage(ctx context.Context, ref Ref, session, id string) error {
	evidence := struct{ ID, Session string }{id, session}
	if err := atomicFile(filepath.Join(s.Root, string(ref.Execution)+"-"+id+".accepted"), mustJSON(evidence)); err != nil {
		return err
	}
	return s.update(ctx, ref, func(j *Journal) error { return j.Messages.Accept(id) })
}

func (s *Server) codexEnd(ctx context.Context, ref Ref, phase, detail string) error {
	if err := s.update(ctx, ref, func(j *Journal) error { j.Phase, j.LastError = phase, detail; return nil }); err != nil {
		return err
	}
	if detail != "" {
		return errors.New(detail)
	}
	return nil
}

// codexTurn runs one codex process to its end. The first turn records the
// thread codex created before any tool call can reach the guard; a resumed
// turn on any other thread is killed. onStart runs once, when the turn has
// started on that thread, and its failure ends the turn.
func (s *Server) codexTurn(ctx context.Context, ref Ref, h HarnessConfig, env []string, log io.Writer, j Journal, prompt, thread string, onStart func() error) (string, bool, error) {
	c := exec.CommandContext(ctx, h.Executable, codexArgs(j.Request.Model, j.Handle.Workspace, thread)...) //nolint:gosec // pinned harness and typed arguments
	if !strings.HasSuffix(prompt, "\n") {
		prompt += "\n"
	}
	c.Dir, c.Env, c.Stdin = j.Handle.Workspace, env, strings.NewReader(prompt)
	// Authentication diagnostics must not leak values into logs or replies.
	c.Stderr = io.Discard
	stdout, err := c.StdoutPipe()
	if err != nil {
		return thread, false, err
	}
	if err := c.Start(); err != nil {
		return thread, false, errors.New("harness process could not start")
	}
	kill := func(err error) (string, bool, error) {
		_ = c.Process.Kill()
		_ = c.Wait()
		return thread, false, err
	}
	accepted, failed := false, false
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 64<<10), MaxWire)
	for scan.Scan() {
		line := scan.Bytes()
		if _, err := log.Write(append(append([]byte(nil), line...), '\n')); err != nil {
			return kill(err)
		}
		var ev struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Type {
		case codexThreadStarted:
			if thread != "" && ev.ThreadID != thread || ev.ThreadID == "" || !ValidIdentity(ev.ThreadID) {
				return kill(errors.New("harness conversation identity mismatch"))
			}
			if thread == "" {
				id := ev.ThreadID
				if err := s.update(ctx, ref, func(j *Journal) error {
					if j.Thread != "" && j.Thread != id {
						return errors.New("harness conversation identity mismatch")
					}
					j.Thread = id
					return nil
				}); err != nil {
					return kill(err)
				}
				thread = id
			}
		case codexTurnFailed:
			failed = true
		case codexTurnStarted:
			if thread == "" {
				return kill(errors.New("codex turn started before its thread"))
			}
			if !accepted {
				if err := onStart(); err != nil {
					return kill(err)
				}
				accepted = true
			}
		}
	}
	if scan.Err() != nil {
		_ = c.Process.Kill()
	}
	err = c.Wait()
	if err == nil {
		err = scan.Err()
	}
	if err == nil && failed {
		err = errors.New("codex reported a failed turn")
	}
	return thread, accepted, err
}
