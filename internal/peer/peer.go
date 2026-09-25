// Package peer sends a message to a running Claude session from the command
// line. Claude Code has no send command, so the message goes through one
// headless `claude -p` turn whose only tool is SendMessage: the same
// messaging a session uses to reach another session on this machine. It
// reaches a session only while that session's CLI runs, and only by the name
// ListAgents shows (a desktop session's title), never by its local_ id.
//
// This rests on Claude Code's own peer messaging, which is not a documented
// interface: the tool's result text is what says whether the message was
// delivered. Everything that depends on it is in this package, and its live
// test (BEEKEEPER_PEER_TEST) runs it against a real session.
package peer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Sender runs the headless turn that sends a message.
type Sender struct {
	// Claude is the claude binary; empty: claude on PATH.
	Claude string
	// Model runs the sender's turn; empty: haiku.
	Model string
	// Dir is the sender's working directory; empty: the current one. The
	// turn loads no settings, plugins, MCP servers or project instructions.
	Dir string
	// Timeout bounds the turn; zero: DefaultTimeout.
	Timeout time.Duration
}

// DefaultTimeout bounds a send: the turn takes about 5 to 15 s.
const DefaultTimeout = 90 * time.Second

// Result is what a send cost and what the tool said.
type Result struct {
	// Reply is the SendMessage tool's result.
	Reply   string
	CostUSD float64
	Elapsed time.Duration
}

// ErrUnreachable is a target whose CLI does not run or whose name no
// running session has.
var ErrUnreachable = errors.New("not reachable from the command line")

const system = "You relay one message. Call the SendMessage tool exactly once with the given to and message, verbatim, then stop. Never call another tool, never retry."

// Send delivers message to the running session named to.
func (s Sender) Send(ctx context.Context, to, message string) (Result, error) {
	if strings.TrimSpace(to) == "" {
		return Result{}, errors.New("no session name to send to")
	}
	bin, model, timeout := s.Claude, s.Model, s.Timeout
	if bin == "" {
		bin = "claude"
	}
	if model == "" {
		model = "haiku"
	}
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := json.Marshal(map[string]string{"to": to, "message": message})
	if err != nil {
		return Result{}, err
	}
	c := exec.CommandContext(ctx, bin, "-p", //nolint:gosec // the configured claude binary; the message is one argument
		"--model", model,
		"--tools", "SendMessage",
		"--allowedTools", "SendMessage",
		"--strict-mcp-config",
		"--setting-sources", "",
		"--no-session-persistence",
		"--system-prompt", system,
		"--output-format", "stream-json", "--verbose",
		"SendMessage "+string(req))
	c.Dir = s.Dir
	c.Env = senderEnv(os.Environ())
	var stderr bytes.Buffer
	c.Stderr = &stderr
	start := time.Now()
	out, err := c.Output()
	r, perr := parse(out)
	r.Elapsed = time.Since(start)
	switch {
	case ctx.Err() != nil:
		return r, fmt.Errorf("sending to %q: %w after %s", to, ctx.Err(), timeout)
	case perr != nil:
		return r, fmt.Errorf("sending to %q: %w", to, perr)
	case err != nil && r.Reply == "":
		return r, fmt.Errorf("sending to %q: %w: %s", to, err, strings.TrimSpace(stderr.String()))
	}
	return r, verdict(to, r.Reply)
}

// senderEnv drops the variables a Claude session hands its tool commands, so
// the sender is a session of its own and not taken for the caller's child.
func senderEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CLAUDECODE" || strings.HasPrefix(k, "CLAUDE_CODE_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "MAX_THINKING_TOKENS=0")
}

// verdict reads the SendMessage result: {"success":true,...} is a message
// queued in the target session, which runs it as its next turn (a
// [Cross-session delivery notice] would follow only to the sender, which has
// exited, if the target held it for its user's approval or refused it).
func verdict(to, reply string) error {
	var res struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal([]byte(reply), &res) == nil && res.Success {
		return nil
	}
	l := strings.ToLower(reply)
	if strings.Contains(l, "no agent named") || strings.Contains(l, "not reachable") {
		return fmt.Errorf("%q: %w (%s)", to, ErrUnreachable, firstLine(reply))
	}
	return fmt.Errorf("%q: not sent: %s", to, firstLine(reply))
}

// parse reads the turn's stream: the SendMessage tool result and the cost.
func parse(out []byte) (Result, error) {
	var r Result
	calls := 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		var ev struct {
			Type    string  `json:"type"`
			Cost    float64 `json:"total_cost_usd"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "result":
			r.CostUSD = ev.Cost
		case "user":
			var blocks []struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(ev.Message.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				if b.Type == "tool_result" {
					calls++
					r.Reply = text(b.Content)
				}
			}
		}
	}
	if calls == 0 {
		return r, errors.New("the sender called no SendMessage")
	}
	return r, nil
}

// text flattens a tool result's content: a string or text blocks.
func text(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &blocks)
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n")
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
