package agy

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
)

// streamEvent is the subset of an agy stream-json stdout event that the
// runner inspects to track turn boundaries and conversation ids.
type streamEvent struct {
	Event          string `json:"event"`
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

// linkConversation records the mapping between convID and inbox so that
// beekeeper hook preinvocation resolves convID to the session's inbox and
// session discovery resolves the agent start id to convID.
func linkConversation(inbox, convID string) {
	c := cleanID(convID)
	if c == "" {
		return
	}
	stateDir := filepath.Dir(filepath.Dir(inbox))
	link := ConvLinkPath(stateDir, c)
	_ = os.MkdirAll(filepath.Dir(link), 0o700)
	tmp := link + ".tmp"
	if os.WriteFile(tmp, []byte(inbox+"\n"), 0o600) == nil {
		_ = os.Rename(tmp, link)
	}
	if id := cleanID(strings.TrimSuffix(filepath.Base(inbox), ".in")); id != "" {
		convFile := AgentConvPath(stateDir, id)
		ctmp := convFile + ".tmp"
		if os.WriteFile(ctmp, []byte(c+"\n"), 0o600) == nil {
			_ = os.Rename(ctmp, convFile)
		}
	}
}

// Run executes argv (an `agy --input-format stream-json --output-format
// stream-json` command) with its stream output appended to logPath and its
// turns fed from the persistent inbox at inbox.
//
// While agy is idle (after its init event or after a turn's result event),
// Run pops the next queued message from inbox into Inflight and writes it as
// a stream-json user event to agy's stdin. While a turn is running, Run does
// not write to agy's stdin: queued messages stay in the persistent queue for
// the PreInvocation hook to drain into Injected before the next model call
// inside the running turn, and each user_input step_update event confirms
// the corresponding Inflight or Injected message. Any Injected messages that
// were not confirmed before the turn ends (e.g. a hook killed or timed out
// on the final model call) are restored to Messages on result and sent on
// stdin as the next turn.
func Run(ctx context.Context, inbox, logPath string, argv []string, stderr io.Writer) error {
	if len(argv) == 0 {
		return errors.New("agy-run: missing command")
	}
	if err := MakeInbox(inbox); err != nil {
		return err
	}
	if err := restoreInflight(inbox); err != nil {
		return err
	}
	fifo, err := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // the agent's inbox under the state folder
	if err != nil {
		return err
	}
	defer func() { _ = fifo.Close() }()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the agent's stream log under the state folder
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv from the unit beekeeper started
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	events := make(chan streamEvent, 16)
	go func() {
		defer close(events)
		r := bufio.NewReaderSize(stdoutPipe, 64<<10)
		for {
			line, rerr := r.ReadBytes('\n')
			if len(line) > 0 {
				var ev streamEvent
				if json.Unmarshal(line, &ev) == nil && ev.Event != "" {
					if ev.Event == eventInit && ev.ConversationID != "" {
						linkConversation(inbox, ev.ConversationID)
					}
					_, _ = logFile.Write(line)
					events <- ev
				} else {
					_, _ = logFile.Write(line)
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	wakeCh := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 256)
		for {
			n, rerr := fifo.Read(buf)
			if n > 0 {
				select {
				case wakeCh <- struct{}{}:
				default:
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	ready, busy := false, false
	feedIfIdle := func() {
		if !ready || busy {
			return
		}
		msg, ok, perr := popInflight(inbox)
		if perr != nil || !ok {
			return
		}
		line, lerr := streamLine(msg)
		if lerr != nil {
			return
		}
		if _, werr := stdinPipe.Write(line); werr != nil {
			return
		}
		busy = true
	}

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				_ = stdinPipe.Close()
				err := cmd.Wait()
				_ = fifo.Close()
				return err
			}
			switch ev.Event {
			case eventInit:
				ready, busy = true, false
				feedIfIdle()
			case "step_update":
				if ev.StepUpdate != nil && ev.StepUpdate.StepType == "user_input" {
					_ = Confirm(inbox)
				}
			case "result":
				_ = finishTurn(inbox)
				busy = false
				feedIfIdle()
			}
		case <-wakeCh:
			feedIfIdle()
		}
	}
}
