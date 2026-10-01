package guard

import (
	"slices"
	"strings"
	"testing"
)

const (
	guideSession = "guide-session"
	guidePerson  = "Timo"
	scratch      = "/tmp/x"
)

// guideHook is a hook whose guide runs in guideSession.
func guideHook() Hook {
	h := hook()
	h.Guide = func(session string) (bool, string) { return session == guideSession, guidePerson }
	return h
}

// guideCalls are calls that do work, by the tool and its input.
var guideCalls = map[string]map[string]any{
	"edit":            toolEvent("Edit", map[string]any{filePathKey: scratch, "old_string": "a", "new_string": "b"}),
	"write":           toolEvent("Write", map[string]any{filePathKey: scratch}),
	"notebook":        toolEvent("NotebookEdit", map[string]any{"notebook_path": "/tmp/x.ipynb"}),
	"commit":          bashCall("git add -A && git commit -m 'docs: x'"),
	"commit -C":       bashCall("git -C ~/repo commit -am x"),
	"push":            bashCall("cd ~/repo; git push origin HEAD"),
	"push in sh -c":   bashCall("zsh -c 'git push'"),
	"devctl merge":    bashCall("devctl pr merge o/r 7"),
	"devctl path":     bashCall("~/go/bin/devctl pr merge o/r 7"),
	"devctl promote":  bashCall("devctl release promote o/r"),
	"gh merge":        bashCall("gh pr merge 7 --repo o/r --squash"),
	"gh review":       bashCall("gh pr review 7 --repo o/r --approve"),
	"browser click":   toolEvent(browserTools+"computer", map[string]any{"action": "left_click", "coordinate": []any{1, 2}}),
	"browser type":    computerCall("type"),
	"browser form":    toolEvent(browserTools+"form_input", map[string]any{"ref": "r1", "value": "x"}),
	"browser js":      toolEvent(browserTools+"javascript_tool", map[string]any{}),
	"connector merge": toolEvent("mcp__github__merge_pull_request", map[string]any{"pullNumber": 7}),
	"connector review": toolEvent("mcp__claude_ai_GitHub_MCP__pull_request_review_write",
		map[string]any{"method": "create"}),
}

// guideReads are calls the guide makes: they read, ask or relay.
var guideReads = map[string]map[string]any{
	"git log":          bashCall("git log --oneline -5 && git status"),
	"grep commit":      bashCall("grep -n 'git commit' notes.md"),
	"gh pr view":       bashCall("gh pr view 7 --repo o/r && gh pr checks 7 --repo o/r"),
	"devctl pr list":   bashCall("devctl pr list o/r"),
	"beekeeper":        bashCall("beekeeper guide next && beekeeper note answer 3 'yes'"),
	"note add":         bashCall("beekeeper note add --for Timo 'merge?'"),
	"read":             toolEvent("Read", map[string]any{filePathKey: scratch}),
	"send":             toolEvent(SendMessageTool, map[string]any{"to": "Agent one", "message": "Timo says yes"}),
	"browser navigate": toolEvent(browserTools+"navigate", map[string]any{"url": "https://example.com"}),
	"browser read":     toolEvent(browserTools+"get_page_text", map[string]any{"tabId": 1}),
	"browser shot":     computerCall("screenshot"),
	"browser scroll":   computerCall("scroll"),
	"connector read":   toolEvent("mcp__github__get_pull_request_reviews", map[string]any{"pullNumber": 7}),
}

func bashCall(cmd string) map[string]any {
	return toolEvent(bashTool, map[string]any{commandKey: cmd})
}

func computerCall(action string) map[string]any {
	return toolEvent(browserTools+"computer", map[string]any{actionKey: action})
}

func withSession(ev map[string]any, session string) map[string]any {
	out := make(map[string]any, len(ev)+1)
	for k, v := range ev {
		out[k] = v
	}
	out["session_id"] = session
	return out
}

func TestGuideIsRefusedWork(t *testing.T) {
	h := guideHook()
	for name, ev := range guideCalls {
		d := decideEvent(t, h, withSession(ev, guideSession))
		if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "Hand it to the supervisor in one line") {
			t.Errorf("%s in the guide's session: want the delegation refusal, got %+v", name, d)
		}
	}
}

func TestGuideMayAskRelayAndRead(t *testing.T) {
	h := guideHook()
	for name, ev := range guideReads {
		if d := decideEvent(t, h, withSession(ev, guideSession)); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("%s in the guide's session is refused: %s", name, d.Reason)
		}
	}
}

func TestOtherSessionsMayWork(t *testing.T) {
	h := guideHook()
	for name, ev := range guideCalls {
		if d := decideEvent(t, h, withSession(ev, "worker-session")); d != nil && strings.Contains(d.Reason, "the guide asks and relays") {
			t.Errorf("%s in a worker's session gets the guide's refusal: %s", name, d.Reason)
		}
	}
	// No guide lookup: nothing is the guide's.
	h.Guide = nil
	if d := decideEvent(t, h, withSession(guideCalls["edit"], guideSession)); d != nil {
		t.Errorf("without a guide lookup an Edit is decided: %+v", d)
	}
}

func TestGuideLookupOnlyForWork(t *testing.T) {
	h := hook()
	looked := 0
	h.Guide = func(string) (bool, string) { looked++; return true, guidePerson }
	for _, ev := range guideReads {
		decideEvent(t, h, withSession(ev, guideSession))
	}
	if looked != 0 {
		t.Errorf("the guide was looked up %d times for calls that do no work", looked)
	}
}

func TestParseQuestionReadsItsParts(t *testing.T) {
	q := parseQuestion(map[string]any{
		"question": "Merge https://github.com/o/r/pull/7? Status quo: CI is green. Why: it changes the release. Checked: gh pr checks",
		"options":  []any{map[string]any{"label": "merge", "description": "it ships today"}, map[string]any{"label": "wait"}},
	})
	want := Question{Text: "Merge https://github.com/o/r/pull/7?", StatusQuo: "CI is green.", Why: "it changes the release.", Checked: "gh pr checks",
		Options: []string{"merge: it ships today", "wait: "}}
	if q.Text != want.Text || q.StatusQuo != want.StatusQuo || q.Why != want.Why || q.Checked != want.Checked || !slices.Equal(q.Options, want.Options) {
		t.Fatalf("parsed %+v, want %+v", q, want)
	}
}

func TestGuidesQuestionsAreChecked(t *testing.T) {
	h := guideHook()
	var got []Question
	h.CheckQuestion = func(q Question) []string {
		got = append(got, q)
		if q.StatusQuo == "" {
			return []string{`--status-quo "<what is true now>"`}
		}
		return nil
	}
	ask := func(session string, qs ...string) *decision {
		var questions []any
		for _, q := range qs {
			questions = append(questions, map[string]any{"question": q, "options": []any{}})
		}
		return decideEvent(t, h, withSession(toolEvent(AskTool, map[string]any{"questions": questions}), session))
	}
	if d := ask(guideSession, "Merge it? Status quo: open. Why: risky."); d != nil {
		t.Errorf("a complete question is refused: %+v", d)
	}
	d := ask(guideSession, "Merge it? Status quo: open.", "And this?")
	if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "question 2 lacks: --status-quo") ||
		strings.Contains(d.Reason, "question 1") || !strings.Contains(d.Reason, `"Status quo: …" part`) {
		t.Fatalf("a question without its status quo: want a refusal naming it, got %+v", d)
	}
	// A worker's question is refused as before, without the check.
	got = nil
	if d := ask("worker-session", "Merge it?"); d == nil || !strings.Contains(d.Reason, "beekeeper note add --for Timo") || got != nil {
		t.Errorf("a worker's question: want the note refusal unchecked, got %+v (checked %v)", d, got)
	}
}
