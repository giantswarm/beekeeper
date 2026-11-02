package execution

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// Run owns one headless turn and its first-task acceptance evidence. Only the
// execution-run service calls it; caller verification is done by the command.
func (s *Server) Run(ctx context.Context, ref Ref) (runErr error) {
	j, err := s.Read(ref)
	if err != nil {
		return err
	}
	if j.Phase != Activating || j.StopRequest != nil {
		return errors.New("execution is not awaiting its first managed turn")
	}
	p, err := s.InstalledPolicy()
	if err != nil || p.Digest() != j.Request.PolicyDigest {
		return errors.New("execution rules or guards changed before launch")
	}
	co, ok := s.Config.Coordinators[j.Request.Coordinator]
	if !ok {
		return errors.New("execution coordinator is not trusted")
	}
	if err := Verify(j.Request, co.PublicKey, s.now()); err != nil {
		return err
	}
	if err := s.authorize(ctx, j.Request); err != nil {
		return err
	}
	started, err := proc.Started(os.Getpid())
	if err != nil {
		return errors.New("cannot establish owned runner process identity")
	}
	if err := s.update(ctx, ref, func(j *Journal) error {
		if j.Phase != Activating || j.StopRequest != nil {
			return errors.New("execution was stopped or already launched")
		}
		j.Phase, j.InTurn, j.LastActive = Running, true, s.now()
		j.RunnerPID, j.RunnerStarted = os.Getpid(), started
		return nil
	}); err != nil {
		return err
	}
	defer func() {
		if runErr == nil {
			return
		}
		// A SIGTERM reaches the runner only through a stop of its own owned
		// unit, and both owners of that stop — the coordinator's stop and a
		// lease expiry — record their stop intent durably before the unit is
		// stopped. A runner whose harness dies with its own termination is
		// carrying that stop out, not crashing: it records the orderly exit
		// with the recorded stop reason and exits 0, so a failed unit is not
		// mistaken for a crash and the journal names the stop. The terminal
		// write continues past the runner's own termination and waits on the
		// journal lock instead of dropping as "execution journal is busy".
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		stopping := ctx.Err() != nil
		stopped := false
		err := s.update(cleanup, ref, func(j *Journal) error {
			j.InTurn, j.LastActive = false, s.now()
			if stopping && j.StopRequest != nil {
				stopped = true
				j.Phase, j.LastError = Exited, "stopped: "+j.StopRequest.Reason
				return nil
			}
			// A failure the runner already journaled keeps its own reason.
			if !j.StopAcknowledged && j.StopRequest == nil && (j.Phase != Failed || j.LastError == "") {
				j.Phase, j.LastError = Failed, "owned harness turn failed; inspect its process state and logs"
			}
			return nil
		})
		if err == nil && stopped {
			runErr = nil
			return
		}
		runErr = errors.Join(runErr, err)
	}()
	leaseStarted := time.Now()
	var lease *LeaseGrant
	if j.Request.LeaseRequired {
		lease, err = s.renewLease(ctx, ref)
		if err != nil {
			return err
		}
	}
	if lease != nil {
		leaseCtx, stopLease := context.WithCancel(ctx)
		leaseDone := make(chan struct{})
		go func() { defer close(leaseDone); s.maintainLease(leaseCtx, ref, leaseStarted, lease) }()
		defer func() { stopLease(); <-leaseDone }()
	}
	if j.Request.Harness == HarnessCodex {
		return s.runCodex(ctx, ref, j)
	}
	h := s.Config.Harnesses[j.Request.Harness]
	env := append(cleanEnvironment(h.ConfigDir, s.Executable), EnvID+"="+string(ref.Execution), EnvCoordinator+"="+j.Request.Coordinator, "BEEKEEPER_CONFIG="+s.ConfigPath)
	tools := "Bash,Read,Write,Edit,Grep,Glob"
	args := []string{"--restricted", "--print", "--session-id", j.Handle.ConversationID, "--model", j.Request.Model, "--name", j.Request.Name, "--permission-mode", "acceptEdits", "--tools", tools, "--allowedTools", tools, "--settings", filepath.Join(s.Root, "settings.json"), "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--output-format", "stream-json", "--verbose", "--append-system-prompt-file", filepath.Join(s.Root, "CLAUDE.md")}
	c := exec.CommandContext(ctx, h.Executable, args...) //nolint:gosec // pinned harness and typed arguments; no daemon or resume
	c.Dir, c.Env, c.Stdin = j.Handle.Workspace, env, strings.NewReader(j.Request.Prompt)
	var stream *messageStream
	if j.Request.Messaging {
		args = append(args, "--input-format", "stream-json", "--replay-user-messages")
		c.Args = append([]string{h.Executable}, args...)
		stream, err = s.openMessageStream(ctx, ref)
		if err != nil {
			return err
		}
		defer stream.Close()
		c.Stdin = stream.reader
	}
	log, err := s.openLog(ref)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	// Authentication diagnostics must not leak values into logs or replies.
	c.Stderr = io.Discard
	if err := c.Start(); err != nil {
		_ = s.update(ctx, ref, func(j *Journal) error {
			j.Phase, j.InTurn, j.LastActive, j.LastError = Failed, false, s.now(), "harness process could not start"
			return nil
		})
		return errors.New("harness process could not start")
	}
	if stream != nil {
		stream.Start(j.Request)
	}
	ready := false
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 64<<10), MaxWire)
	for scan.Scan() {
		line := scan.Bytes()
		if _, err := log.Write(append(append([]byte(nil), line...), '\n')); err != nil {
			_ = c.Process.Kill()
			_ = c.Wait()
			return err
		}
		var ev struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			SessionID string `json:"session_id"`
		}
		if stream != nil {
			if err := stream.Accept(line, j); err != nil {
				_ = c.Process.Kill()
				_ = c.Wait()
				return err
			}
		}
		if json.Unmarshal(line, &ev) == nil {
			switch {
			case ev.Type == "system" && ev.Subtype == "init":
				if ev.SessionID != j.Handle.ConversationID {
					_ = c.Process.Kill()
					_ = c.Wait()
					return errors.New("harness conversation identity mismatch")
				}
				if !ready && stream == nil {
					if err := s.update(ctx, ref, func(j *Journal) error {
						j.Ready, j.InTurn, j.LastActive = true, true, s.now()
						return nil
					}); err != nil {
						_ = c.Process.Kill()
						_ = c.Wait()
						return err
					}
					ready = true
				}
			case ev.Type == "result":
				if err := s.update(ctx, ref, func(j *Journal) error {
					j.InTurn, j.LastActive = j.Messages.hasUncertain(), s.now()
					return nil
				}); err != nil {
					_ = c.Process.Kill()
					_ = c.Wait()
					return err
				}
			}
		}
		if stream != nil && acceptedMessage(line, j.Handle.ConversationID, j.Request.RequestID) {
			ready = true
		}
	}
	if scan.Err() != nil {
		_ = c.Process.Kill()
	}
	err = c.Wait()
	phase, detail := Exited, ""
	if err != nil || scan.Err() != nil || !ready {
		phase, detail = Failed, "harness failed or ended before first-task acceptance"
	}
	if updateErr := s.update(ctx, ref, func(j *Journal) error {
		j.Phase, j.InTurn, j.LastActive, j.LastError = phase, false, s.now(), detail
		return nil
	}); updateErr != nil {
		return updateErr
	}
	if detail != "" {
		return errors.New(detail)
	}
	return nil
}

// openLog opens the owned, append-only execution log.
func (s *Server) openLog(ref Ref) (*os.File, error) {
	logPath := filepath.Join(s.Root, string(ref.Execution)+".log")
	if fi, e := os.Lstat(logPath); e == nil && !fi.Mode().IsRegular() {
		return nil, errors.New("invalid owned execution log")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	return os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // owned execution log
}
