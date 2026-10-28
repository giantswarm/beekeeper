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
		j.Phase = Running
		j.RunnerPID, j.RunnerStarted = os.Getpid(), started
		return nil
	}); err != nil {
		return err
	}
	defer func() {
		if runErr == nil {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err := s.update(cleanup, ref, func(j *Journal) error {
			if !j.StopAcknowledged {
				j.Phase, j.LastError = Failed, "owned harness turn failed; inspect its process state and logs"
			}
			return nil
		})
		runErr = errors.Join(runErr, err)
	}()
	h := s.Config.Harnesses[j.Request.Harness]
	env := append(cleanEnvironment(h.ConfigDir), EnvID+"="+string(ref.Execution), EnvCoordinator+"="+j.Request.Coordinator, "BEEKEEPER_CONFIG="+s.ConfigPath)
	tools := "Bash,Read,Write,Edit,Grep,Glob"
	args := []string{"--restricted", "--print", "--session-id", j.Handle.ConversationID, "--model", j.Request.Model, "--name", j.Request.Name, "--permission-mode", "acceptEdits", "--tools", tools, "--allowedTools", tools, "--settings", filepath.Join(s.Root, "settings.json"), "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--output-format", "stream-json", "--verbose", "--append-system-prompt-file", filepath.Join(s.Root, "CLAUDE.md")}
	c := exec.CommandContext(ctx, h.Executable, args...) //nolint:gosec // pinned harness and typed arguments; no daemon or resume
	c.Dir, c.Env, c.Stdin = j.Handle.Workspace, env, strings.NewReader(j.Request.Prompt)
	logPath := filepath.Join(s.Root, string(ref.Execution)+".log")
	if fi, e := os.Lstat(logPath); e == nil && !fi.Mode().IsRegular() {
		return errors.New("invalid owned execution log")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // owned execution log
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
		_ = s.update(ctx, ref, func(j *Journal) error { j.Phase, j.LastError = Failed, "harness process could not start"; return nil })
		return errors.New("harness process could not start")
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
		if json.Unmarshal(line, &ev) == nil && ev.Type == "system" && ev.Subtype == "init" {
			if ev.SessionID != j.Handle.ConversationID {
				_ = c.Process.Kill()
				_ = c.Wait()
				return errors.New("harness conversation identity mismatch")
			}
			if !ready {
				if err := s.update(ctx, ref, func(j *Journal) error { j.Ready = true; return nil }); err != nil {
					_ = c.Process.Kill()
					_ = c.Wait()
					return err
				}
				ready = true
			}
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
	if updateErr := s.update(ctx, ref, func(j *Journal) error { j.Phase, j.LastError = phase, detail; return nil }); updateErr != nil {
		return updateErr
	}
	if detail != "" {
		return errors.New(detail)
	}
	return nil
}
