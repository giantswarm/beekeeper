// Package agy manages Antigravity CLI (agy) agent sessions: their inbox,
// stream-json runner and PreInvocation hook for mid-turn message delivery.
package agy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Harness is the harness name agy sessions carry (claude.Session.Harness).
const Harness = "agy"

// Comm is the agy binary's process name.
const Comm = "agy"

// The environment beekeeper starts an agy agent with: its start id and its
// roster name.
const (
	EnvAgent = "BEEKEEPER_AGY_AGENT"
	EnvName  = "BEEKEEPER_AGENT_NAME"
)

// HostPrefix makes a start id the agent's host session id on the roster:
// agy_<start id>.
const HostPrefix = "agy_"

// ErrNotRunning is a message to an agent whose inbox no process reads.
var ErrNotRunning = errors.New("no agy process reads its inbox")

// InboxPath is the inbox FIFO of the agent started under id, in beekeeper's
// state folder. Beside it live <inbox>.lock and <inbox>.queue.
func InboxPath(stateDir, id string) string {
	return filepath.Join(stateDir, "agy", id+".in")
}

// StreamPath is the stream-json event log of the agent started under id.
func StreamPath(stateDir, id string) string {
	return filepath.Join(stateDir, "agy", id+".log")
}

// ConvLinkPath maps an agy conversationId to the inbox path of the session
// running that conversation, so the PreInvocation hook can drain the inbox
// by conversationId from its stdin.
func ConvLinkPath(stateDir, convID string) string {
	return filepath.Join(stateDir, "agy", convID+".inbox")
}

// queue is the persistent on-disk message queue beside an inbox (<inbox>.queue).
type queue struct {
	// Inflight is the message written to agy's stdin when idle that agy
	// has not yet recorded in its conversation or started a model
	// invocation for. If the session restarts before accepting it, the
	// runner restores Inflight to the front of Messages.
	Inflight string   `json:"inflight,omitempty"`
	Messages []string `json:"messages,omitempty"`
}

func lockPath(inbox string) string  { return inbox + ".lock" }
func queuePath(inbox string) string { return inbox + ".queue" }

// streamMessage is the stream-json user message line agy reads from stdin
// when idle between turns.
type streamMessage struct {
	Event   string        `json:"event"`
	Message streamContent `json:"message"`
}

type streamContent struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func streamLine(msg string) ([]byte, error) {
	b, err := json.Marshal(streamMessage{
		Event:   "user",
		Message: streamContent{Role: "user", Content: strings.TrimSpace(msg)},
	})
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// PreInvocationArgs are the fields beekeeper hook preinvocation reads from
// agy's PreInvocation stdin payload.
type PreInvocationArgs struct {
	ConversationID string `json:"conversationId"`
	InvocationNum  int    `json:"invocationNum"`
}

// PreInvocationResult is the JSON payload beekeeper hook preinvocation writes
// to stdout when queued messages are injected into a running turn.
type PreInvocationResult struct {
	InjectSteps []InjectedStep `json:"injectSteps,omitempty"`
}

// InjectedStep is one step injected before an agy model call.
type InjectedStep struct {
	UserMessage string `json:"userMessage"`
}

// cleanID returns id when it is a single path element, "" otherwise.
func cleanID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\`) {
		return ""
	}
	return id
}

// ResolveInbox finds the inbox path for a session by its agy conversationId
// (from PreInvocation's stdin) or its BEEKEEPER_AGY_AGENT environment variable.
func ResolveInbox(stateDir, convID string) string {
	if c := cleanID(convID); c != "" {
		if b, err := os.ReadFile(ConvLinkPath(stateDir, c)); err == nil { //nolint:gosec // cleaned conversation id under stateDir
			if id := cleanID(strings.TrimSuffix(strings.TrimSpace(string(b)), ".in")); id != "" {
				return InboxPath(stateDir, id)
			}
			if id := cleanID(strings.TrimSuffix(filepath.Base(strings.TrimSpace(string(b))), ".in")); id != "" {
				return InboxPath(stateDir, id)
			}
		}
	}
	if id := cleanID(os.Getenv(EnvAgent)); id != "" {
		p := InboxPath(stateDir, id)
		if _, err := os.Stat(p); err == nil { //nolint:gosec // cleaned agent id under stateDir
			return p
		}
		if _, err := os.Stat(queuePath(p)); err == nil { //nolint:gosec // cleaned agent id under stateDir
			return p
		}
	}
	if c := cleanID(convID); c != "" {
		return InboxPath(stateDir, c)
	}
	return ""
}

// PreInvocation drains the queued messages of the session named in raw (a
// PreInvocation hook stdin payload) and formats them as an injectSteps JSON
// response. It returns nil when there are no messages to inject or raw is
// malformed.
func PreInvocation(stateDir string, raw []byte) []byte {
	var in PreInvocationArgs
	if json.Unmarshal(raw, &in) != nil {
		return nil
	}
	inbox := ResolveInbox(stateDir, in.ConversationID)
	if inbox == "" {
		return nil
	}
	msgs, err := Drain(inbox)
	if err != nil || len(msgs) == 0 {
		return nil
	}
	steps := make([]InjectedStep, len(msgs))
	for i, m := range msgs {
		steps[i] = InjectedStep{UserMessage: m}
	}
	b, err := json.Marshal(PreInvocationResult{InjectSteps: steps})
	if err != nil {
		return nil
	}
	return append(b, '\n')
}
