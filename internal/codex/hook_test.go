//go:build unix

package codex

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestWithInboxContext(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvAgent, "hook-worker")
	inbox := InboxPath(dir, "hook-worker")
	if err := MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	fifo, err := os.OpenFile(inbox, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fifo.Close() }()
	if err := Send(inbox, "active prompt"); err != nil {
		t.Fatal(err)
	}
	if _, err := Drain(inbox); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []string{"first wake", "second wake"} {
		if err := Send(inbox, msg); err != nil {
			t.Fatal(err)
		}
	}
	decision := []byte(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"guard reason","additionalContext":"existing context","updatedInput":{"command":"echo rewritten"}},"other":true}`)
	var wg sync.WaitGroup
	results := make(chan string, 2)
	for range 2 {
		wg.Go(func() { results <- string(WithInboxContext(dir, "", decision)) })
	}
	wg.Wait()
	close(results)
	delivered := 0
	for result := range results {
		var out struct {
			Specific struct {
				Decision string            `json:"permissionDecision"`
				Reason   string            `json:"permissionDecisionReason"`
				Context  string            `json:"additionalContext"`
				Updated  map[string]string `json:"updatedInput"`
			} `json:"hookSpecificOutput"`
			Other bool `json:"other"`
		}
		if err := json.Unmarshal([]byte(result), &out); err != nil {
			t.Fatal(err)
		}
		if out.Specific.Decision != "deny" || out.Specific.Reason != "guard reason" || out.Specific.Updated["command"] != "echo rewritten" || !out.Other {
			t.Fatalf("changed guard decision: %s", result)
		}
		if !strings.HasPrefix(out.Specific.Context, "existing context") {
			t.Fatalf("lost existing context: %s", result)
		}
		if strings.Contains(result, "first wake") {
			delivered++
			if !strings.Contains(result, "second wake") || strings.Contains(result, "active prompt") {
				t.Fatalf("wrong messages: %s", result)
			}
		}
	}
	if delivered != 1 {
		t.Fatalf("wake deliveries = %d, want 1", delivered)
	}
	msgs, err := Drain(inbox)
	if err != nil || len(msgs) != 1 || msgs[0] != "active prompt" {
		t.Fatalf("inflight prompt lost: %v, %v", msgs, err)
	}
	if err := Confirm(inbox); err != nil {
		t.Fatal(err)
	}
	msgs, err = Drain(inbox)
	if err != nil || len(msgs) != 0 {
		t.Fatalf("wake duplicated as next turn: %v, %v", msgs, err)
	}
}

func TestWithInboxContextMalformedDecisionKeepsWake(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvAgent, "hook-worker")
	inbox := InboxPath(dir, "hook-worker")
	if err := withLock(inbox, func() error {
		return writeQueue(inbox, persistentQueue{Messages: []QueuedMessage{{ID: "wake", Text: "keep me"}}})
	}); err != nil {
		t.Fatal(err)
	}
	for _, decision := range []string{`invalid`, `null`, `{"hookSpecificOutput":null}`, `{"hookSpecificOutput":{"additionalContext":42}}`} {
		if got := string(WithInboxContext(dir, "", []byte(decision))); got != decision {
			t.Fatalf("changed malformed decision: %s", got)
		}
	}
	msgs, err := Drain(inbox)
	if err != nil || len(msgs) != 1 || msgs[0] != "keep me" {
		t.Fatalf("wake lost: %v, %v", msgs, err)
	}
}
