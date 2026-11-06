package execution

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// agyTurnFailure is the journal's reason for a turn that failed after its
// acceptance; the coordinator's inspect reports it with the failed phase.
const agyTurnFailure = "agy turn failed after acceptance; inspect agents logs and recover the execution"

// agyArgs is one headless agy turn: the first creates the conversation, every
// later one resumes the conversation the journal recorded. The prompt is fed
// as a stream-json user message on stdin. Permission prompts are skipped as
// for local workers; the execution guard runs as the managed home's PreToolUse
// hook instead.
func agyArgs(model, thread string) []string {
	args := []string{
		"--input-format", streamJSONMode,
		"--output-format", streamJSONMode,
		"--model", model,
		"--dangerously-skip-permissions",
		"--new-project",
	}
	if thread != "" {
		args = append(args, "--conversation", thread)
	}
	return args
}

func agyStreamMessage(text string) string {
	b, _ := json.Marshal(struct {
		Event   string `json:"event"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}{
		Event: userMessageType,
		Message: struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{Role: userMessageType, Content: text},
	})
	return string(b) + "\n"
}

func (s *Server) agyMCPHomeDir(ref Ref) string {
	return filepath.Join(s.Root, string(ref.Execution)+"-agy-home")
}

func (s *Server) prepareAgyMCPHome(ref Ref, j Journal) (string, error) {
	home := s.agyMCPHomeDir(ref)
	cfgDir := filepath.Join(home, ".gemini", "config")
	if err := ownedDirectory(home); err != nil {
		return "", err
	}
	if err := ownedDirectory(filepath.Join(home, ".gemini")); err != nil {
		return "", err
	}
	if err := ownedDirectory(cfgDir); err != nil {
		return "", err
	}
	for _, name := range []string{agyRulesFile, hooksJSONFile, "skills.json"} {
		dst := filepath.Join(cfgDir, name)
		_ = os.Remove(dst)
		target := filepath.Join(s.agyConfigDir(), name)
		if name == agyRulesFile && s.coordinatorBundle != nil {
			target = s.rulesFile()
		}
		if err := os.Symlink(target, dst); err != nil {
			return "", err
		}
	}
	appLink := filepath.Join(home, ".gemini", "antigravity-cli")
	_ = os.Remove(appLink)
	if err := os.Symlink(s.agyAppDir(), appLink); err != nil {
		return "", err
	}
	mcp := mustJSON(map[string]any{
		mcpServersKey: map[string]any{
			"coordinator": map[string]any{
				"command": s.Executable,
				"args":    []string{configFlag, s.ConfigPath, agentsCommand, mcpProxyCommand},
				"env": map[string]string{
					EnvID:          string(ref.Execution),
					EnvCoordinator: j.Request.Coordinator,
				},
			},
		},
	})
	if !j.Request.MCP {
		mcp = mustJSON(map[string]any{mcpServersKey: map[string]any{}})
	}
	if err := atomicFile(filepath.Join(cfgDir, "mcp_config.json"), mcp); err != nil {
		return "", err
	}
	return home, nil
}

// runAgy owns an agy worker for its lifetime: the first turn carries the
// brief, each stored wake runs as the next resumed turn on the recorded
// conversation, and a finished report ends the loop once its turn is over.
func (s *Server) runAgy(ctx context.Context, ref Ref, j Journal) error {
	h := s.Config.Harnesses[HarnessAgy]
	if err := s.linkAgyLogin(); err != nil {
		return errors.New("agy login link in the managed home is unavailable")
	}
	home, err := s.prepareAgyMCPHome(ref, j)
	if err != nil {
		return err
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
	env := append(s.toolPath(s.agyEnvironmentWithHome(home)), EnvID+"="+string(ref.Execution), EnvCoordinator+"="+j.Request.Coordinator, "BEEKEEPER_CONFIG="+s.ConfigPath)
	env, envErr := s.ownedEnvironment(j, env)
	if envErr != nil {
		return envErr
	}
	env, envErr = s.GitHubEnvironment(j, env)
	if envErr != nil {
		return envErr
	}
	var transcriptOff int64
	agyTranscript := func(threadID string) string {
		return filepath.Clean(filepath.Join(s.agyAppDir(), "brain", threadID, ".system_generated", "logs", "transcript.jsonl"))
	}
	mirrorAgyTranscript := func(threadID string) {
		if threadID == "" || !ValidIdentity(threadID) {
			return
		}
		f, err := os.Open(agyTranscript(threadID))
		if err != nil {
			return
		}
		defer func() { _ = f.Close() }()
		if _, err := f.Seek(transcriptOff, io.SeekStart); err != nil {
			return
		}
		if n, err := io.Copy(log, f); err == nil && n > 0 {
			transcriptOff += n
		}
	}
	first := func() error {
		return s.update(ctx, ref, func(j *Journal) error {
			j.Ready, j.InTurn, j.LastActive = true, true, s.now()
			return nil
		})
	}
	thread := j.Thread
	if j.continuing() {
		// The log holds what the last runner mirrored; only new lines follow.
		if fi, err := os.Stat(agyTranscript(thread)); err == nil {
			transcriptOff = fi.Size()
		}
	} else {
		var accepted bool
		var turnErr error
		thread, accepted, turnErr = s.agyTurn(ctx, ref, h, env, log, j, j.Request.Prompt, "", first)
		mirrorAgyTranscript(thread)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !accepted || thread == "" {
			return s.codexEnd(ctx, ref, Failed, "harness failed or ended before first-task acceptance")
		}
		if turnErr != nil {
			return s.codexEnd(ctx, ref, Failed, agyTurnFailure)
		}
		if !j.Request.Messaging {
			return s.codexEnd(ctx, ref, Exited, "")
		}
	}
	if err := s.update(ctx, ref, func(j *Journal) error {
		j.InTurn, j.LastActive = false, s.now()
		return nil
	}); err != nil {
		return err
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
		if err := s.authorizeMessage(ctx, current); err != nil {
			if err := s.update(ctx, ref, func(j *Journal) error {
				j.LastError = "agy message remains stored: coordinating authority unavailable or stopped"
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
		begun, err := s.beginTurn(ctx, ref, func(j *Journal) error {
			if j.StopRequest != nil {
				return errors.New("execution stopped before message delivery")
			}
			if err := j.Messages.Attempt(next); err != nil {
				return err
			}
			j.InTurn, j.LastActive = true, s.now()
			return nil
		})
		if err != nil {
			return err
		}
		if !begun {
			// A host update holds the admission: the message stays
			// stored for the turn after it.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-wake:
			case <-time.After(turnRetry):
			}
			continue
		}
		accept := func() error { return s.acceptCodexMessage(ctx, ref, current.Handle.ConversationID, next.ID) }
		_, accepted, turnErr := s.agyTurn(ctx, ref, h, env, log, current, codexMessageText(next), thread, accept)
		mirrorAgyTranscript(thread)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if accepted && turnErr != nil {
			return s.codexEnd(ctx, ref, Failed, agyTurnFailure)
		}
		if !accepted {
			if err := s.update(ctx, ref, func(j *Journal) error {
				j.InTurn, j.LastActive = false, s.now()
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
			continue
		}
		if err := s.update(ctx, ref, func(j *Journal) error {
			j.InTurn, j.LastActive = false, s.now()
			return nil
		}); err != nil {
			return err
		}
	}
}

// agyTurn runs one agy process for a single turn. The first turn records the
// conversation id agy emits on init before any tool call can reach the guard;
// a resumed turn on any other conversation id is killed. onStart runs once,
// when user_input step_update confirms acceptance on that conversation.
func (s *Server) agyTurn(ctx context.Context, ref Ref, h HarnessConfig, env []string, log io.Writer, j Journal, prompt, thread string, onStart func() error) (string, bool, error) {
	args := agyArgs(j.Request.Model, thread)
	c := exec.CommandContext(ctx, h.Executable, args...) //nolint:gosec // pinned harness and typed arguments
	c.Dir, c.Env = j.Handle.Workspace, env
	// Authentication diagnostics must not leak values into logs or replies.
	c.Stderr = io.Discard
	stdinPipe, err := c.StdinPipe()
	if err != nil {
		return thread, false, err
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		return thread, false, err
	}
	if err := c.Start(); err != nil {
		return thread, false, errors.New("harness process could not start")
	}
	msgLine := agyStreamMessage(prompt)
	written := make(chan struct{})
	go func() {
		defer close(written)
		_, _ = io.WriteString(stdinPipe, msgLine)
	}()
	var closeOnce sync.Once
	closeStdin := func() {
		closeOnce.Do(func() {
			<-written
			_ = stdinPipe.Close()
		})
	}
	kill := func(err error) (string, bool, error) {
		_ = c.Process.Kill()
		closeStdin()
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
			Event          string `json:"event"`
			Type           string `json:"type"`
			ConversationID string `json:"conversation_id"`
			StepUpdate     *struct {
				ConversationID string `json:"conversation_id"`
				StepType       string `json:"step_type"`
			} `json:"step_update"`
			Result *struct {
				ConversationID string `json:"conversation_id"`
				Status         string `json:"status"`
			} `json:"result"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		kind := ev.Event
		if kind == "" {
			kind = ev.Type
		}
		switch kind {
		case "init":
			if thread != "" && ev.ConversationID != thread || ev.ConversationID == "" || !ValidIdentity(ev.ConversationID) {
				return kill(errors.New("harness conversation identity mismatch"))
			}
			if thread == "" {
				id := ev.ConversationID
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
		case "step_update":
			if ev.StepUpdate == nil || ev.StepUpdate.StepType != "user_input" {
				continue
			}
			if thread == "" {
				return kill(errors.New("agy turn started before its conversation"))
			}
			if ev.StepUpdate.ConversationID != "" && ev.StepUpdate.ConversationID != thread {
				return kill(errors.New("harness conversation identity mismatch"))
			}
			if !accepted {
				if err := onStart(); err != nil {
					return kill(err)
				}
				accepted = true
			}
		case "result":
			if ev.Result != nil {
				if ev.Result.ConversationID != "" && ev.Result.ConversationID != thread {
					return kill(errors.New("harness conversation identity mismatch"))
				}
				switch strings.ToLower(strings.TrimSpace(ev.Result.Status)) {
				case "error", "failed", "cancelled", "canceled":
					failed = true
				}
			}
			closeStdin()
		case "error":
			failed = true
			closeStdin()
		}
	}
	closeStdin()
	if scan.Err() != nil {
		_ = c.Process.Kill()
	}
	err = c.Wait()
	if err == nil {
		err = scan.Err()
	}
	if err == nil && failed {
		err = errors.New("agy reported a failed turn")
	}
	return thread, accepted, err
}
