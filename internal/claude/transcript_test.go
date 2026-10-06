package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFirst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	lines := `{"type":"summary"}
{"type":"user","isMeta":true,"message":{"role":"user","content":"meta"}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>x</system-reminder>"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"the brief"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}
{"type":"user","message":{"role":"user","content":"later"}}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	turn, found, err := First(path)
	if err != nil || !found || turn.Text != "the brief" {
		t.Errorf("First = %+v, %v, %v", turn, found, err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"summary"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := First(path); err != nil || found {
		t.Errorf("no user turn: found %v, err %v", found, err)
	}
}

func TestModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	for _, c := range []struct{ name, lines, want string }{
		{"no assistant message yet", `{"type":"custom-title","customTitle":"x"}
{"type":"user","message":{"role":"user","content":"the brief"}}
`, ""},
		{"the last assistant model", `{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001"}}
{"type":"assistant","message":{"model":"<synthetic>"}}
{"type":"user","message":{"role":"user","content":"next"}}
{"type":"assistant","message":{"model":"claude-sonnet-5"}}
`, "claude-sonnet-5"},
		{"a partial last line", `{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001"}}
{"type":"assistant","message":{"model":"claude-sonn`, "claude-haiku-4-5-20251001"},
	} {
		if err := os.WriteFile(path, []byte(c.lines), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := Model(path); err != nil || got != c.want {
			t.Errorf("%s: Model = %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	if _, err := Model(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Error("a missing transcript: no error")
	}
}

func TestCalled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	use := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__claude_ai_Slack__slack_send_message","input":{}}]}}` + "\n"
	for _, c := range []struct {
		name, lines string
		want        bool
	}{
		{"no call", `{"type":"user","message":{"role":"user","content":"the brief"}}` + "\n", false},
		{"a call without its result yet", use, false},
		{"a failed call", use + `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"denied"}]}}` + "\n", false},
		{"another tool's result", `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t0","name":"Bash","input":{}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t0","content":"ok"}]}}
`, false},
		{"a posted message", use + `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"sent"}]}]}}` + "\n", true},
	} {
		if err := os.WriteFile(path, []byte(c.lines), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := Called(path, "slack_send_message"); err != nil || got != c.want {
			t.Errorf("%s: Called = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}

func TestOpenBackground(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	lines := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"make test","description":"Run the tests","run_in_background":true}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"Command running in background with ID: b1."}]},"toolUseResult":{"backgroundTaskId":"b1"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"until gh run view 1; do :; done","run_in_background":true}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"Command running in background with ID: b2."}]},"toolUseResult":{"backgroundTaskId":"b2"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"ls"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t3","content":"a"}]},"toolUseResult":{"stdout":"a"}}
{"type":"queue-operation","operation":"enqueue","content":"<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n</task-notification>"}
{"type":"user","message":{"role":"user","content":"<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n</task-notification>"}}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := OpenBackground(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Command != "until gh run view 1; do :; done" {
		t.Errorf("OpenBackground = %+v, want only the wait no notice followed", got)
	}
}

// An answer holds only what the session did from the message on: its tool
// calls with their results, and its last text.
func TestReadAnswer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	lines := []string{
		`{"type":"assistant","timestamp":"2026-10-05T10:00:00Z","message":{"content":[{"type":"text","text":"before"}]}}`,
		`{"type":"user","timestamp":"2026-10-05T10:01:00Z","message":{"content":"please archive"}}`,
		`{"type":"assistant","timestamp":"2026-10-05T10:01:01Z","message":{"content":[{"type":"tool_use","id":"t1","name":"mcp__ccd_session_mgmt__archive_session","input":{"session_id":"local_x"}}]}}`,
		`{"type":"user","timestamp":"2026-10-05T10:01:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"Archived session local_x"}]}]}}`,
		`{"type":"assistant","timestamp":"2026-10-05T10:01:03Z","message":{"content":[{"type":"text","text":"Done."}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := ReadAnswer(path, time.Date(2026, 10, 5, 10, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if a.Text != "Done." || len(a.Calls) != 1 {
		t.Fatalf("answer %+v", a)
	}
	if c := a.Calls[0]; !c.Done || c.Error || c.Result != "Archived session local_x" || !strings.HasSuffix(c.Name, "archive_session") {
		t.Errorf("call %+v", c)
	}
}
