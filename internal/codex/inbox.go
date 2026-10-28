package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// ErrNotRunning is returned by Send when no codex runner holds the inbox FIFO
// open for reading.
var ErrNotRunning = errors.New("codex session is not running")

// Session states written to StatePath(inbox) by the runner and read by Discover.
const (
	StateBusy = "busy"
	StateIdle = "idle"
)

// InboxPath is the FIFO inbox path for a beekeeper-started codex agent id.
func InboxPath(stateDir, id string) string {
	return filepath.Join(stateDir, "codex", id+".in")
}

// LogPath is the JSONL stream log path for a beekeeper-started codex agent id.
func LogPath(stateDir, id string) string {
	return filepath.Join(stateDir, "codex", id+".jsonl")
}

// ThreadPath is the file recording the Codex thread_id of a beekeeper-started
// codex agent id.
func ThreadPath(stateDir, id string) string {
	return filepath.Join(stateDir, "codex", id+".thread")
}

// ThreadLinkPath maps a Codex thread_id back to the agent's inbox path.
func ThreadLinkPath(stateDir, threadID string) string {
	return filepath.Join(stateDir, "codex", "threads", filepath.Base(threadID))
}

// QueuePath is the persistent JSON queue backing inbox.
func QueuePath(inbox string) string { return inbox + ".queue" }

// LockPath is the flock file protecting QueuePath(inbox).
func LockPath(inbox string) string { return inbox + ".lock" }

// StatePath is the state file ("busy" or "idle") for inbox.
func StatePath(inbox string) string { return inbox + ".state" }

// QueuedMessage is one wake message tracked in the persistent inbox queue with
// a unique ID so retries and restarts deduplicate already-delivered messages.
type QueuedMessage struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// persistentQueue is the on-disk JSON state of an inbox's unread messages.
// Inflight holds the message currently handed to codex's stdin until the
// runner observes turn.started or turn.completed on stdout; if codex crashes
// before accepting it, the runner restores Inflight to the front of Messages.
// Injected holds messages returned by the PreToolUse hook until the runner
// confirms the tool call started or the turn completed.
type persistentQueue struct {
	Messages  []QueuedMessage `json:"messages,omitempty"`
	Inflight  string          `json:"inflight,omitempty"`
	InflightID string         `json:"inflightId,omitempty"`
	Injected  []QueuedMessage `json:"injected,omitempty"`
	Delivered []string        `json:"delivered,omitempty"`
	ThreadID  string          `json:"threadId,omitempty"`
}

func normalizeQueue(q *persistentQueue) {
	for i := range q.Messages {
		if q.Messages[i].ID == "" {
			q.Messages[i].ID = uuid.NewString()
		}
	}
	for i := range q.Injected {
		if q.Injected[i].ID == "" {
			q.Injected[i].ID = uuid.NewString()
		}
	}
}

func isDelivered(q *persistentQueue, id string) bool {
	return id != "" && slices.Contains(q.Delivered, id)
}

func markDelivered(q *persistentQueue, ids ...string) {
	for _, id := range ids {
		if id != "" && !slices.Contains(q.Delivered, id) {
			q.Delivered = append(q.Delivered, id)
		}
	}
	const maxDelivered = 128
	if len(q.Delivered) > maxDelivered {
		q.Delivered = q.Delivered[len(q.Delivered)-maxDelivered:]
	}
}

func restoreUnconfirmedInjected(q *persistentQueue) {
	if len(q.Injected) == 0 {
		return
	}
	var pending []QueuedMessage
	for _, m := range q.Injected {
		if !isDelivered(q, m.ID) {
			pending = append(pending, m)
		}
	}
	q.Injected = nil
	if len(pending) > 0 {
		q.Messages = append(pending, q.Messages...)
	}
}

func readQueue(inbox string) (persistentQueue, error) {
	var q persistentQueue
	b, err := os.ReadFile(QueuePath(inbox))
	if errors.Is(err, os.ErrNotExist) || len(bytes.TrimSpace(b)) == 0 {
		return q, nil
	}
	if err != nil {
		return q, err
	}
	if err := json.Unmarshal(b, &q); err != nil {
		return persistentQueue{}, err
	}
	normalizeQueue(&q)
	return q, nil
}

func writeQueue(inbox string, q persistentQueue) error {
	qpath := QueuePath(inbox)
	if len(q.Messages) == 0 && q.Inflight == "" && len(q.Injected) == 0 && len(q.Delivered) == 0 && q.ThreadID == "" {
		if err := os.Remove(qpath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.Marshal(q)
	if err != nil {
		return err
	}
	return writeAtomic(qpath, b)
}

// WriteState writes the session state ("busy" or "idle") for inbox.
func WriteState(inbox, st string) {
	_ = writeAtomic(StatePath(inbox), []byte(st+"\n"))
}

// ReadState reads the session state ("busy" or "idle") for inbox.
func ReadState(inbox string) string {
	b, err := os.ReadFile(StatePath(inbox))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// RecordThread records the Codex thread_id for the session owning inbox.
func RecordThread(stateDir, inbox, threadID string) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil
	}
	if err := withLock(inbox, func() error {
		q, err := readQueue(inbox)
		if err != nil {
			return err
		}
		q.ThreadID = threadID
		return writeQueue(inbox, q)
	}); err != nil {
		return err
	}
	if stateDir != "" {
		id := strings.TrimSuffix(filepath.Base(inbox), ".in")
		_ = writeAtomic(ThreadPath(stateDir, id), []byte(threadID+"\n"))
		_ = writeAtomic(ThreadLinkPath(stateDir, threadID), []byte(inbox+"\n"))
	}
	return nil
}

// ReadThread returns the recorded Codex thread_id for inbox, or "".
func ReadThread(stateDir, inbox string) string {
	var threadID string
	_ = withLock(inbox, func() error {
		q, err := readQueue(inbox)
		if err == nil {
			threadID = q.ThreadID
		}
		return nil
	})
	if threadID != "" {
		return threadID
	}
	if stateDir != "" {
		id := strings.TrimSuffix(filepath.Base(inbox), ".in")
		if b, err := os.ReadFile(ThreadPath(stateDir, id)); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// ResolveInbox finds the FIFO inbox path for a codex session from its
// BEEKEEPER_CODEX_AGENT environment variable or its thread_id link file.
func ResolveInbox(stateDir, threadID string) string {
	if id := strings.TrimSpace(os.Getenv(EnvAgent)); id != "" && stateDir != "" {
		return InboxPath(stateDir, id)
	}
	if threadID != "" && stateDir != "" {
		b, err := os.ReadFile(ThreadLinkPath(stateDir, threadID))
		if err == nil {
			if p := strings.TrimSpace(string(b)); p != "" {
				return p
			}
		}
	}
	return ""
}

// preToolUseHookInput is the minimal subset of Codex's PreToolUse stdin JSON
// needed to resolve the session's inbox.
type preToolUseHookInput struct {
	SessionID string `json:"session_id"`
}

// preToolUseHookResponse is Codex's PreToolUse stdout JSON when injecting
// mid-turn wake messages via additionalContext.
type preToolUseHookResponse struct {
	HookSpecificOutput preToolUseSpecificOutput `json:"hookSpecificOutput"`
}

type preToolUseSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// DrainHookText drains pending wake messages from the session's inbox for a
// PreToolUse hook invocation and returns the joined additionalContext string
// (empty when no messages are queued).
func DrainHookText(stateDir string, stdin []byte) string {
	var in preToolUseHookInput
	_ = json.Unmarshal(stdin, &in)
	inbox := ResolveInbox(stateDir, in.SessionID)
	if inbox == "" {
		return ""
	}
	msgs, err := DrainHook(inbox)
	if err != nil || len(msgs) == 0 {
		return ""
	}
	texts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if strings.TrimSpace(m.Text) != "" {
			texts = append(texts, m.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

// PreToolUse reads a Codex PreToolUse hook event on stdin, drains any queued
// wake messages from the session's inbox, and returns the PreToolUse JSON
// response carrying additionalContext (or nil when no wake message is queued).
func PreToolUse(stateDir string, stdin []byte) []byte {
	ctxText := DrainHookText(stateDir, stdin)
	if ctxText == "" {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(preToolUseHookResponse{
		HookSpecificOutput: preToolUseSpecificOutput{
			HookEventName:     "PreToolUse",
			AdditionalContext: ctxText,
		},
	}); err != nil {
		return nil
	}
	return buf.Bytes()
}
