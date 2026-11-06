package peer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
)

// QueueInstruction stores a relay independently of Claude's peer notification.
// The notification wakes an idle CLI; hooks deliver the instruction itself.
func QueueInstruction(stateDir, session, text string) (string, error) {
	if session == "" {
		return "", errors.New("no relay session")
	}
	dir := instructionDir(stateDir, session)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".pending-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.WriteString(text); err != nil {
		_ = f.Close()
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%020d-%s.msg", time.Now().UnixNano(), uuid.NewString()))
	return path, os.Rename(f.Name(), path)
}

func instructionDir(stateDir, session string) string {
	sum := sha256.Sum256([]byte(session))
	return filepath.Join(stateDir, "peer-inbox", hex.EncodeToString(sum[:]))
}

// InstructionStatus reports only hook consumption, never notification acceptance.
func InstructionStatus(path string) string {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return "delivered"
	}
	return "queued"
}

// InstructionHook drains only this session's relays at its next tool or turn
// boundary. Stop blocks once with the instructions, so delivery does not wait
// for another user prompt. The lock serializes concurrent hook invocations.
func InstructionHook(stateDir, session, event string) ([]byte, error) {
	if session == "" {
		return nil, nil
	}
	switch event {
	case "PreToolUse", "UserPromptSubmit", "Stop":
	default:
		return nil, nil
	}
	dir := instructionDir(stateDir, session)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	lock := flock.New(filepath.Join(dir, ".lock"))
	if err := lock.Lock(); err != nil {
		return nil, err
	}
	defer func() { _ = lock.Unlock() }()
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var paths, texts []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".msg") {
			continue
		}
		path := entry.Name()
		text, err := root.ReadFile(path)
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
		texts = append(texts, string(text))
	}
	if len(paths) == 0 {
		return nil, nil
	}
	context := "Relayed instructions:\n" + strings.Join(texts, "\n\n")
	var output any = map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": event, "additionalContext": context}}
	if event == "Stop" {
		output = map[string]string{"decision": "block", "reason": context}
	}
	raw, err := json.Marshal(output)
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		if err := root.Remove(path); err != nil {
			return nil, err
		}
	}
	return append(raw, '\n'), nil
}

// WithInstructionContext preserves the guard's decision while adding pending
// relay text. Validate the output before consuming any instructions.
func WithInstructionContext(stateDir string, event, decision []byte) []byte {
	var ev struct {
		Session string `json:"session_id"`
	}
	if json.Unmarshal(event, &ev) != nil {
		return decision
	}
	out := map[string]any{}
	if len(decision) > 0 && (json.Unmarshal(decision, &out) != nil || out == nil) {
		return decision
	}
	specific := map[string]any{"hookEventName": "PreToolUse"}
	if v, ok := out["hookSpecificOutput"]; ok {
		var valid bool
		specific, valid = v.(map[string]any)
		if !valid || specific == nil {
			return decision
		}
	}
	prior, _ := specific["additionalContext"].(string)
	raw, err := InstructionHook(stateDir, ev.Session, "PreToolUse")
	if err != nil || raw == nil {
		return decision
	}
	var delivery struct {
		Specific struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal(raw, &delivery) != nil {
		return decision
	}
	specific["additionalContext"] = strings.TrimSpace(prior + "\n\n" + delivery.Specific.Context)
	out["hookSpecificOutput"] = specific
	encoded, err := json.Marshal(out)
	if err != nil {
		return decision
	}
	return append(encoded, '\n')
}
