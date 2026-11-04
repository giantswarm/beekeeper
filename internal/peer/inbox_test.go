package peer

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestInstructionAtNextBoundary(t *testing.T) {
	for _, event := range []string{"PreToolUse", "UserPromptSubmit", "Stop"} {
		t.Run(event, func(t *testing.T) {
			dir := t.TempDir()
			path, err := QueueInstruction(dir, "waiting-session", "Click the OAuth consent")
			if err != nil {
				t.Fatal(err)
			}
			if got := InstructionStatus(path); got != "queued" {
				t.Fatalf("status %s", got)
			}
			if out, err := InstructionHook(dir, "other-session", event); err != nil || len(out) != 0 {
				t.Fatalf("other session: %s, %v", out, err)
			}
			out, err := InstructionHook(dir, "waiting-session", event)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if json.Unmarshal(out, &decoded) != nil || !strings.Contains(string(out), "Click the OAuth consent") {
				t.Fatalf("delivery: %s", out)
			}
			if event == "Stop" && decoded["decision"] != "block" {
				t.Fatalf("turn ended without instruction: %s", out)
			}
			if got := InstructionStatus(path); got != "delivered" {
				t.Fatalf("status %s", got)
			}
			if out, err := InstructionHook(dir, "waiting-session", event); err != nil || len(out) != 0 {
				t.Fatalf("duplicate: %s, %v", out, err)
			}
		})
	}
}

func TestConcurrentInstructionBoundariesDeliverOnce(t *testing.T) {
	dir := t.TempDir()
	if _, err := QueueInstruction(dir, "session", "consent now"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan string, 8)
	for range 8 {
		wg.Go(func() {
			out, err := InstructionHook(dir, "session", "Stop")
			if err != nil {
				t.Error(err)
			}
			results <- string(out)
		})
	}
	wg.Wait()
	close(results)
	count := 0
	for out := range results {
		if strings.Contains(out, "consent now") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("delivered %d times", count)
	}
}

func TestInstructionPreservesGuardDecision(t *testing.T) {
	dir := t.TempDir()
	path, err := QueueInstruction(dir, "session", "consent now")
	if err != nil {
		t.Fatal(err)
	}
	event := []byte(`{"session_id":"session"}`)
	malformed := []byte(`not JSON`)
	if got := WithInstructionContext(dir, event, malformed); string(got) != string(malformed) || InstructionStatus(path) != "queued" {
		t.Fatal("malformed decision consumed instruction")
	}
	decision := []byte(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"guard refusal","additionalContext":"existing context"}}`)
	out := WithInstructionContext(dir, event, decision)
	for _, want := range []string{"deny", "guard refusal", "existing context", "consent now"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
}
