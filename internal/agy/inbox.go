// Package agy manages Antigravity CLI (agy) agent sessions: their inbox,
// stream-json runner and PreInvocation hook for mid-turn message delivery.
package agy

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// QueuedMessage is one message in an agy session's persistent inbox, carrying
// a unique ID so at-least-once delivery across retries and restarts can
// deduplicate messages and deliver each one in FIFO order once.
type QueuedMessage struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	InputSeq int    `json:"inputSeq,omitempty"`
}

// queue is the persistent on-disk message queue beside an inbox (<inbox>.queue).
type queue struct {
	// NextSeq is the monotonic counter used when assigning message IDs.
	NextSeq int `json:"nextSeq,omitempty"`
	// StreamInputs is the number of user_input step_update events the
	// current agy-run process has seen on stdout.
	StreamInputs int `json:"streamInputs,omitempty"`
	// Transcript is the conversation's transcript.jsonl path, and
	// TranscriptInputs is the number of USER_INPUT steps in that
	// transcript before the current Inflight/Injected messages were sent.
	Transcript       string `json:"transcript,omitempty"`
	TranscriptInputs int    `json:"transcriptInputs,omitempty"`
	// Inflight is the message written to agy's stdin when idle that agy
	// has not yet recorded in its conversation. If the session restarts
	// before accepting it, the runner restores Inflight to the front of
	// Messages.
	Inflight *QueuedMessage `json:"inflight,omitempty"`
	// Injected holds the messages drained by PreInvocation for a mid-turn
	// model call that have not yet been confirmed in the stream or
	// transcript. Until confirmed, a subsequent PreInvocation re-injects
	// them and a turn end or session restart restores them to the front of
	// Messages.
	Injected []QueuedMessage `json:"injected,omitempty"`
	// Messages are the pending messages waiting to be delivered, in FIFO
	// order.
	Messages []QueuedMessage `json:"messages,omitempty"`
	// Delivered holds the IDs of messages already confirmed delivered in
	// this session, so a retried or restored message is never delivered
	// twice.
	Delivered []string `json:"delivered,omitempty"`
}

const maxDeliveredIDs = 64

func (q *queue) markDelivered(id string) {
	if id == "" || slices.Contains(q.Delivered, id) {
		return
	}
	q.Delivered = append(q.Delivered, id)
	if len(q.Delivered) > maxDeliveredIDs {
		q.Delivered = q.Delivered[len(q.Delivered)-maxDeliveredIDs:]
	}
}

func dedupMessages(delivered []string, lists ...[]QueuedMessage) []QueuedMessage {
	seen := make(map[string]bool, len(delivered))
	for _, id := range delivered {
		if id != "" {
			seen[id] = true
		}
	}
	var out []QueuedMessage
	for _, list := range lists {
		for _, m := range list {
			if m.ID != "" {
				if seen[m.ID] {
					continue
				}
				seen[m.ID] = true
			}
			out = append(out, m)
		}
	}
	return out
}

type transcriptLine struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

func transcriptUserInputs(path string) ([]string, bool) {
	if path == "" {
		return nil, false
	}
	f, err := os.Open(filepath.Clean(path)) //nolint:gosec // transcript path from agy hook or session state
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var line transcriptLine
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.Type == "USER_INPUT" {
			out = append(out, line.Content)
		}
	}
	if sc.Err() != nil {
		return nil, false
	}
	return out, true
}

// reconcile confirms any Inflight or Injected messages that have appeared in
// the stream (StreamInputs) or in the transcript (USER_INPUT steps at or after
// TranscriptInputs), moving their IDs to Delivered. It returns the highest
// InputSeq confirmed so far.
func (q *queue) reconcile(transcriptPath string) int {
	if transcriptPath != "" {
		q.Transcript = transcriptPath
	}
	highSeq := q.StreamInputs
	if q.Inflight != nil && q.Inflight.InputSeq > 0 && q.Inflight.InputSeq <= q.StreamInputs {
		q.markDelivered(q.Inflight.ID)
		q.Inflight = nil
		q.TranscriptInputs++
	}
	for len(q.Injected) > 0 && q.Injected[0].InputSeq > 0 && q.Injected[0].InputSeq <= q.StreamInputs {
		q.markDelivered(q.Injected[0].ID)
		q.Injected = q.Injected[1:]
		q.TranscriptInputs++
	}
	inputs, ok := transcriptUserInputs(q.Transcript)
	if !ok {
		return highSeq
	}
	if q.Inflight != nil && q.TranscriptInputs < len(inputs) && strings.Contains(inputs[q.TranscriptInputs], q.Inflight.Text) {
		if q.Inflight.InputSeq > highSeq {
			highSeq = q.Inflight.InputSeq
		}
		q.markDelivered(q.Inflight.ID)
		q.Inflight = nil
		q.TranscriptInputs++
	}
	for len(q.Injected) > 0 && q.TranscriptInputs < len(inputs) {
		if !strings.Contains(inputs[q.TranscriptInputs], q.Injected[0].Text) {
			break
		}
		if q.Injected[0].InputSeq > highSeq {
			highSeq = q.Injected[0].InputSeq
		}
		q.markDelivered(q.Injected[0].ID)
		q.Injected = q.Injected[1:]
		q.TranscriptInputs++
	}
	return highSeq
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
	TranscriptPath string `json:"transcriptPath"`
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

func defaultTranscriptPath(convID string) string {
	c := cleanID(convID)
	if c == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	p := filepath.Join(home, ".gemini", "antigravity-cli", "brain", c, ".system_generated", "logs", "transcript.jsonl")
	if _, err := os.Stat(p); err == nil { //nolint:gosec // cleaned conversation id under user home
		return p
	}
	return ""
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

// PreInvocation marks the queued messages of the session named in raw (a
// PreInvocation hook stdin payload) inflight in its persistent queue and
// formats them as an injectSteps JSON response. Messages remain inflight on
// disk until confirmed by the stream or transcript so a killed or timed-out
// hook or an agy crash before storing the step re-injects or restores them
// without message loss or duplicate delivery.
func PreInvocation(stateDir string, raw []byte) []byte {
	var in PreInvocationArgs
	if json.Unmarshal(raw, &in) != nil {
		return nil
	}
	inbox := ResolveInbox(stateDir, in.ConversationID)
	if inbox == "" {
		return nil
	}
	tp := in.TranscriptPath
	if tp == "" {
		tp = defaultTranscriptPath(in.ConversationID)
	}
	msgs, err := drainHook(inbox, tp)
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
