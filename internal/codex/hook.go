package codex

import (
	"encoding/json"
	"strings"
)

// WithInboxContext adds pending wakes to a PreToolUse decision. The runner's
// inflight stdin prompt belongs to the current turn and must never be drained
// by a hook. Both consumers share the queue lock, so a wake goes to either a
// tool hook or a resumed turn, never both. A malformed decision or queue leaves
// messages pending and preserves the guard's original response.
func WithInboxContext(stateDir, threadID string, decision []byte) []byte {
	inbox := ResolveInbox(stateDir, threadID)
	if inbox == "" {
		return decision
	}
	var out map[string]json.RawMessage
	if json.Unmarshal(decision, &out) != nil || out == nil {
		return decision
	}
	specific := map[string]json.RawMessage{}
	if raw, ok := out["hookSpecificOutput"]; ok {
		if json.Unmarshal(raw, &specific) != nil || specific == nil {
			return decision
		}
	}
	var prior string
	if raw, ok := specific["additionalContext"]; ok {
		if json.Unmarshal(raw, &prior) != nil {
			return decision
		}
	}
	var result []byte
	err := withLock(inbox, func() error {
		q, err := readQueue(inbox)
		if err != nil {
			return err
		}
		contexts := []string{}
		if prior != "" {
			contexts = append(contexts, prior)
		}
		ids := []string{}
		for _, msg := range q.Messages {
			if isDelivered(&q, msg.ID) {
				continue
			}
			contexts = append(contexts, "Message from beekeeper agents wake:\n"+msg.Text)
			ids = append(ids, msg.ID)
		}
		if len(ids) == 0 {
			return nil
		}
		specific["hookEventName"], _ = json.Marshal("PreToolUse")
		specific["additionalContext"], _ = json.Marshal(strings.Join(contexts, "\n\n"))
		out["hookSpecificOutput"], err = json.Marshal(specific)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			return err
		}
		q.Messages = nil
		markDelivered(&q, ids...)
		if err := writeQueue(inbox, q); err != nil {
			return err
		}
		result = append(encoded, '\n')
		return nil
	})
	if err != nil || result == nil {
		return decision
	}
	return result
}
