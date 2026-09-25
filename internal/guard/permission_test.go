package guard

import (
	"encoding/json"
	"testing"
)

func TestPermissionAllowsOnlyBeekeepersImportedBypassStarts(t *testing.T) {
	const ours, theirs = "0b7e6a52-0000-4000-8000-000000000001", "0b7e6a52-0000-4000-8000-000000000002"
	asked := 0
	started := func(s string) bool { asked++; return s == ours }
	event := func(session, mode string) []byte {
		raw, _ := json.Marshal(map[string]any{"hook_event_name": "PermissionRequest", "session_id": session,
			"permission_mode": mode, "tool_name": "WebSearch", "tool_input": map[string]any{"query": "example"}})
		return raw
	}
	for _, c := range []struct {
		name  string
		input []byte
		allow bool
		asks  int
	}{
		{"beekeeper's start, dropped to acceptEdits by the import", event(ours, ModeAcceptEdits), true, 1},
		{"a session beekeeper did not start", event(theirs, ModeAcceptEdits), false, 1},
		{"beekeeper's start lowered to default", event(ours, "default"), false, 0},
		{"beekeeper's start in plan mode", event(ours, "plan"), false, 0},
		{"beekeeper's start still in bypass", event(ours, "bypassPermissions"), false, 0},
		{"beekeeper's start in auto", event(ours, "auto"), false, 0},
		{"no session id", event("", ModeAcceptEdits), false, 0},
		{"another hook event", []byte(`{"hook_event_name":"PreToolUse","session_id":"` + ours + `","permission_mode":"acceptEdits"}`), false, 0},
		{"malformed input", []byte(`{"hook_event_name":`), false, 0},
		{"empty input", nil, false, 0},
	} {
		asked = 0
		out, r := Permission(c.input, started)
		if (out != nil) != c.allow || asked != c.asks {
			t.Errorf("%s: out = %s, asked the record %d times, want allow %v and %d", c.name, out, asked, c.allow, c.asks)
		}
		if c.allow && r.Tool != "WebSearch" {
			t.Errorf("%s: tool = %q", c.name, r.Tool)
		}
	}
	out, _ := Permission(event(ours, ModeAcceptEdits), started)
	var got struct {
		HookSpecificOutput struct {
			HookEventName string `json:"hookEventName"`
			Decision      struct {
				Behavior string `json:"behavior"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &got); err != nil || got.HookSpecificOutput.HookEventName != "PermissionRequest" || got.HookSpecificOutput.Decision.Behavior != "allow" {
		t.Errorf("answer = %s (%v)", out, err)
	}
}
