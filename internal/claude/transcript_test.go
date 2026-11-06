package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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

// TurnDone says whether the transcript's last message of its own answers the
// last message and stopped: the proof that no turn runs. A wake's message
// (a user line the model has not answered), a tool call round, a line the
// CLI still writes, a subagent's line, an unknown or missing file are none
// of them proof.
func TestTurnDone(t *testing.T) {
	const (
		userMsg = `{"type":"user","timestamp":"2026-10-05T10:00:00Z","message":{"content":"go on"}}`
		// the wake's message: a message entry, not a tool result
		userBlock = `{"type":"user","timestamp":"2026-10-05T10:00:00Z","message":{"content":[{"type":"text","text":"go on"}]}}`
		toolCall  = `{"type":"assistant","timestamp":"2026-10-05T10:00:01Z","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`
		toolRet   = `{"type":"user","timestamp":"2026-10-05T10:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"ok"}]}]}}`
		answered  = `{"type":"assistant","timestamp":"2026-10-05T10:00:03Z","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}}`
		thinking  = `{"type":"assistant","timestamp":"2026-10-05T10:00:01Z","message":{"stop_reason":"tool_use","content":[{"type":"thinking","thinking":"hmm"}]}}`
		sidechain = `{"type":"assistant","isSidechain":true,"timestamp":"2026-10-05T10:00:03Z","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"sub"}]}}`
		noReason  = `{"type":"assistant","timestamp":"2026-10-05T10:00:03Z","message":{"content":[{"type":"text","text":"writing"}]}}`
	)
	for _, c := range []struct {
		name  string
		lines []string // "" is a line only partly written (no newline after it)
		want  bool
	}{
		{"empty", nil, false},
		{"no message", []string{`{"type":"summary","summary":"x"}`}, false},
		{"wake's message, model has not answered", []string{answered, userMsg}, false},
		{"wake's message as a content block", []string{answered, userBlock}, false},
		{"thinking mid-turn, the next round is coming", []string{answered, userMsg, thinking}, false},
		{"tool call round", []string{answered, userMsg, toolCall}, false},
		{"tool result, model has not answered", []string{answered, userMsg, toolCall, toolRet}, false},
		{"turn answered and stopped", []string{answered, userMsg, toolCall, toolRet, answered}, true},
		{"answer ends the turn, tool round before it", []string{answered, userMsg, toolCall, toolRet, toolCall, toolRet, answered}, true},
		{"the answer's line still written", []string{answered, userMsg, toolCall, toolRet, `{"type":"assistant","timestamp":"2026-10-05T10:00:03Z","message":{"stop_reason":"end`}, false},
		{"subagent's answer is not the session's", []string{answered, userMsg, sidechain}, false},
		{"a no-stop-reason line holds the stop off", []string{userMsg, noReason}, false},
		{"meta line is no turn", []string{answered, `{"type":"user","isMeta":true,"message":{"content":"<system-reminder>x</system-reminder>"}}`, answered}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := strings.Join(c.lines, "\n")
			if len(c.lines) > 0 && !strings.HasPrefix(c.name, "the answer's line") {
				body += "\n"
			}
			path := filepath.Join(t.TempDir(), "s.jsonl")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := TurnDone(path); got != c.want {
				t.Errorf("TurnDone = %v, want %v for %q", got, c.want, body)
			}
		})
	}
	if TurnDone(filepath.Join(t.TempDir(), "missing.jsonl")) || TurnDone("") {
		t.Error("a missing transcript or no path proves a turn ended")
	}
}

func TestTailIncludesAskUserQuestionAnswersAndNotes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	lines := []string{
		`{"type":"assistant","timestamp":"2026-10-05T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{"questions":[{"question":"Merge?"}]}}]}}`,
		`{"type":"user","timestamp":"2026-10-05T10:00:05Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"q1","content":"User has answered your questions: \"Merge?\"=\"Merge\" user's notes: \"please wait for CI first\". You can now continue with the user's answers in mind."}]},"toolUseResult":{"answers":{"Merge?":"Merge"},"annotations":{"Merge?":{"notes":"please wait for CI first"}}}}`,
		`{"type":"user","timestamp":"2026-10-05T10:00:06Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"q2","content":"ignored timeout"}]},"toolUseResult":{"answers":{"Merge?":"Merge"},"afkTimeoutMs":5000}}`,
		`{"type":"assistant","timestamp":"2026-10-05T10:00:06Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","timestamp":"2026-10-05T10:00:06Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"b1","content":"The user responded: not a dialog"}]},"toolUseResult":{"response":"not a person","notes":"neither"}}`,
		`{"type":"assistant","timestamp":"2026-10-05T10:00:06Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"q3","name":"AskUserQuestion","input":{"questions":[{"question":"Approach?"}]}}]}}`,
		`{"type":"user","timestamp":"2026-10-05T10:00:07Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"q3","content":"User has answered your questions: \"Approach?\"=\"Option A\" user's notes: \"keep the existing flag\"."}]}}`,
		`{"type":"user","timestamp":"2026-10-05T10:00:08Z","message":{"role":"user","content":"also update the docs"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	turns, err := Tail(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	var userTexts []string
	for _, turn := range turns {
		if turn.Role == roleUser {
			userTexts = append(userTexts, turn.Text)
		}
	}
	want := []string{
		"Merge — please wait for CI first",
		"Option A — keep the existing flag",
		"also update the docs",
	}
	if len(userTexts) != len(want) {
		t.Fatalf("user turns = %q, want %q", userTexts, want)
	}
	for i := range want {
		if userTexts[i] != want[i] {
			t.Errorf("user turn[%d] = %q, want %q", i, userTexts[i], want[i])
		}
	}

	dialogs, err := DialogsSince(path, time.Date(2026, 10, 5, 10, 0, 6, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(dialogs) != 1 || dialogs[0].Text != "Option A — keep the existing flag" || !slices.Equal(dialogs[0].Questions, []string{"Approach?"}) {
		t.Fatalf("DialogsSince = %+v, want the Approach? answer alone", dialogs)
	}
}

func TestAskUserQuestionFallbackReaders(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"single answer", `"Proceed?"="Wait for CI"`, "Wait for CI"},
		{"multiple answers", `"Proceed?"="Wait for CI", "Scope?"="Keep the current flag"`, `"Proceed?"="Wait for CI", "Scope?"="Keep the current flag"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.jsonl")
			lines := []string{
				`{"type":"assistant","timestamp":"2026-10-05T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{"questions":[{"question":"Proceed?"},{"question":"Scope?"}]}}]}}`,
			}
			result, err := json.Marshal("User has answered your questions: " + tc.body + ". You can now continue with the user's answers in mind.")
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, `{"type":"user","timestamp":"2026-10-05T10:00:05Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"q1","content":`+string(result)+`}]}}`)
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			turns, err := Tail(path, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(turns) != 1 || turns[0].Role != roleUser || turns[0].Text != tc.want {
				t.Errorf("Tail = %+v, want user text %q", turns, tc.want)
			}
			dialogs, err := DialogsSince(path, time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			if len(dialogs) != 1 || dialogs[0].Text != tc.want || !slices.Equal(dialogs[0].Questions, []string{"Proceed?", "Scope?"}) {
				t.Errorf("DialogsSince = %+v, want both questions and text %q", dialogs, tc.want)
			}
		})
	}
}

func TestTailAskUserQuestion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	lines := strings.Join([]string{
		`{"type":"assistant","timestamp":"2026-10-05T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{"questions":[{"question":"Which retention policy?"}]}}]}}`,
		`{"type":"user","timestamp":"2026-10-05T10:00:05Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"q1","content":"User has answered your questions: \"Which retention policy?\"=\"Keep 30 days with 2 GiB cap\"."}]},"toolUseResult":{"answers":{"Which retention policy?":"Keep 30 days with 2 GiB cap"}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	turns, err := Tail(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Role != roleUser || !strings.Contains(turns[0].Text, "Keep 30 days with 2 GiB cap") {
		t.Fatalf("Tail = %+v, want AskUserQuestion answer as user turn", turns)
	}
	followed, err := Follow(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(followed) != 2 || followed[0].Role != RoleTool || followed[0].Text != "AskUserQuestion: Which retention policy?" {
		t.Fatalf("Follow = %+v, want AskUserQuestion tool turn and user answer", followed)
	}
}

func TestTailKeepsDialogAlongsideText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mixed.jsonl")
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{"questions":[{"question":"Proceed?"}]}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"additional instruction"},{"type":"tool_result","tool_use_id":"q1","content":"The user responded: Wait"}]},"toolUseResult":{"answers":{"Proceed?":"Wait"},"notes":"build the button first"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	turns, err := Tail(path, 10)
	if err != nil || len(turns) != 1 || turns[0].Text != "additional instruction\nWait — build the button first" {
		t.Fatalf("Tail = %+v, %v", turns, err)
	}
	dialogs, err := DialogsSince(path, time.Time{})
	if err != nil || len(dialogs) != 1 || dialogs[0].Text != "Wait — build the button first" {
		t.Fatalf("DialogsSince = %+v, %v", dialogs, err)
	}
}
