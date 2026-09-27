package guard

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func decideSend(t *testing.T, h Hook, to string) *decision {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"tool_name": SendMessageTool, "tool_input": map[string]any{"to": to, "message": "hi", "summary": "s"}})
	out := h.Decide(raw)
	if out == nil {
		return nil
	}
	var o struct {
		D decision `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &o); err != nil {
		t.Fatalf("hook output %q: %v", out, err)
	}
	return &o.D
}

func TestSendMessageToARunningDesktopSessionGoesByName(t *testing.T) {
	var asked string
	h := Hook{Peer: func(host string) (string, error) { asked = host; return "gpm worker", nil }}
	d := decideSend(t, h, "local_abc")
	if d == nil || d.PermissionDecision != decisionAllow {
		t.Fatalf("want a redirect, got %+v", d)
	}
	if asked != "local_abc" || d.UpdatedInput["to"] != "gpm worker" || d.UpdatedInput["message"] != "hi" || d.UpdatedInput["summary"] != "s" {
		t.Fatalf("redirect %+v (asked %q)", d.UpdatedInput, asked)
	}
}

func TestSendMessagePasses(t *testing.T) {
	called := false
	h := Hook{Peer: func(string) (string, error) { called = true; return "", nil }}
	if d := decideSend(t, h, "local_stopped"); d != nil {
		t.Fatalf("a stopped desktop session is the desktop's to start: %+v", d)
	}
	called = false
	if d := decideSend(t, h, "gpm worker"); d != nil || called {
		t.Fatalf("a send by name passes without a lookup: %+v, looked up %v", d, called)
	}
	if d := decideSend(t, Hook{}, "local_x"); d != nil {
		t.Fatalf("no peer lookup: pass, got %+v", d)
	}
}

func TestSendMessageRefusedWhenTheNameIsAmbiguous(t *testing.T) {
	h := Hook{Peer: func(string) (string, error) { return "", errors.New("2 running CLIs are named \"w\"") }}
	d := decideSend(t, h, "local_x")
	if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "2 running CLIs") {
		t.Fatalf("want a refusal, got %+v", d)
	}
}
