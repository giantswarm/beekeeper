package execution

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"time"
)

func (s *Server) probeOmpGuard(ctx context.Context, h HarnessConfig) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, h.Executable, append(s.ompArgs(""), "--no-session")...) //nolint:gosec // pinned harness and typed flags
	c.Dir, c.Env, c.Stderr = s.Root, s.ompEnvironment(), io.Discard
	in, err := c.StdinPipe()
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	if err := c.Start(); err != nil {
		return errors.New("omp guard probe could not start")
	}
	defer func() { cancel(); _ = c.Wait() }()
	scan := bufio.NewScanner(out)
	scan.Buffer(make([]byte, 64<<10), MaxWire)
	for scan.Scan() {
		var ev struct {
			Type    string `json:"type"`
			Session string `json:"session_id"`
		}
		if json.Unmarshal(scan.Bytes(), &ev) != nil {
			continue
		}
		if ev.Type == "extension_error" {
			return errors.New("omp guard extension failed to load")
		}
		if ev.Type == "beekeeper_guard_ready" && ValidIdentity(ev.Session) {
			return nil
		}
	}
	return errors.New("omp guard readiness probe failed")
}

func ompMessage(m Envelope, _ string) []byte {
	return append(mustJSON(map[string]any{"id": m.ID, "type": "prompt", "message": codexMessageText(m), "streamingBehavior": "steer"}), '\n')
}

// runOmp never submits a model prompt until the explicit guard extension has
// started. RPC admission responses carry the durable message id: writes alone
// do not establish acceptance, and reconnection never replays uncertain input.
func (s *Server) runOmp(ctx context.Context, ref Ref, j Journal) error {
	h := s.Config.Harnesses[HarnessOmp]
	stream, err := s.openMessageStream(ctx, ref)
	if err != nil {
		return err
	}
	defer stream.Close()
	stream.encode = ompMessage
	c := exec.CommandContext(ctx, h.Executable, s.ompArgs(j.Request.Model)...) //nolint:gosec // pinned harness and typed arguments
	c.Dir, c.Env, c.Stdin, c.Stderr = j.Handle.Workspace, append(s.ompEnvironment(), EnvID+"="+string(ref.Execution), EnvCoordinator+"="+j.Request.Coordinator, "BEEKEEPER_CONFIG="+s.ConfigPath), stream.reader, io.Discard
	log, err := s.openLog(ref)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	if err := c.Start(); err != nil {
		return s.codexEnd(ctx, ref, Failed, "omp process could not start")
	}
	defer func() { _ = c.Process.Kill(); _ = c.Wait() }()
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 64<<10), MaxWire)
	ready, accepted := false, false
	// Startup has a bound even if an extension never reports its readiness.
	startup := time.AfterFunc(30*time.Second, func() { _ = c.Process.Kill() })
	defer startup.Stop()
	for scan.Scan() {
		line := scan.Bytes()
		if _, err := log.Write(append(append([]byte(nil), line...), '\n')); err != nil {
			return err
		}
		var ev struct {
			Type    string `json:"type"`
			Session string `json:"session_id"`
			ID      string `json:"id"`
			Command string `json:"command"`
			Success bool   `json:"success"`
			Status  string `json:"status"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.Type == "extension_error" || ev.Type == "rpc_frame_error" {
			return s.codexEnd(ctx, ref, Failed, "omp extension or transport failed")
		}
		if ev.Type == "beekeeper_guard_ready" {
			if ready || !ValidIdentity(ev.Session) {
				return s.codexEnd(ctx, ref, Failed, "omp conversation identity mismatch")
			}
			if err := s.update(ctx, ref, func(j *Journal) error { j.Thread = ev.Session; return nil }); err != nil {
				return err
			}
			ready = true
			stream.Start(j.Request)
		}
		if ev.Type == "response" && ev.Command == "prompt" {
			if !ready || !ev.Success {
				return s.codexEnd(ctx, ref, Failed, "omp rejected prompt admission")
			}
			if ev.ID == j.Request.RequestID {
				if accepted {
					continue
				}
				if err := s.update(ctx, ref, func(j *Journal) error { j.Ready, j.InTurn, j.LastActive = true, true, s.now(); return nil }); err != nil {
					return err
				}
				accepted = true
				startup.Stop()
			} else {
				if err := s.acceptCodexMessage(ctx, ref, j.Handle.ConversationID, ev.ID); err != nil {
					return err
				}
			}
			stream.signal()
		}
		if ev.Type == "agent_start" || ev.Type == "agent_end" {
			if !ready {
				return s.codexEnd(ctx, ref, Failed, "omp started before guard readiness")
			}
			if err := s.update(ctx, ref, func(j *Journal) error {
				j.InTurn, j.LastActive = ev.Type == "agent_start" || j.Messages.hasUncertain(), s.now()
				return nil
			}); err != nil {
				return err
			}
			current, err := s.Read(ref)
			if err != nil {
				return err
			}
			if ev.Type == "agent_end" && current.StopRequest != nil {
				return s.codexEnd(ctx, ref, Exited, "")
			}
		}
		if ev.Type == "prompt_result" {
			if ev.Status != "completed" {
				return s.codexEnd(ctx, ref, Failed, "omp prompt failed after admission")
			}
			current, err := s.Read(ref)
			if err != nil {
				return err
			}
			if current.Completed || !j.Request.Messaging {
				return s.codexEnd(ctx, ref, Exited, "")
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return s.codexEnd(ctx, ref, Failed, "omp stream ended unexpectedly; inspect logs and uncertain receipts")
}
