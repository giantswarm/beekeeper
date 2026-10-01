package guard

import (
	"strings"
	"testing"
)

func TestHookLeavesQuestionsToTheGuide(t *testing.T) {
	const guide = "guide-session"
	h := hook()
	h.Guide = func(session string) (bool, string) { return session == guide, "Timo" }
	askFrom := func(h Hook, session string) *decision {
		ev := map[string]any{"session_id": session, "questions": []any{"Which one?"}}
		ev["tool_name"] = AskTool
		return decideEvent(t, h, ev)
	}
	if d := askFrom(h, guide); d != nil {
		t.Errorf("the guide's question is refused: %+v", d)
	}
	d := askFrom(h, "worker-session")
	if d == nil || d.PermissionDecision != decisionDeny {
		t.Fatalf("a worker's question: want a refusal, got %+v", d)
	}
	if !strings.Contains(d.Reason, `beekeeper note add --for Timo "<the question, every issue or PR as its full URL>" --status-quo "<what is true now>" --why`) {
		t.Errorf("the refusal does not name the note:\n%s", d.Reason)
	}
	// No guide running, or no lookup: still refused.
	h.Guide = func(string) (bool, string) { return false, "" }
	if d := askFrom(h, guide); d == nil || !strings.Contains(d.Reason, "--for <person>") {
		t.Errorf("without a guide: want a refusal, got %+v", d)
	}
	h.Guide = nil
	if d := askFrom(h, guide); d == nil || d.PermissionDecision != decisionDeny {
		t.Errorf("without a lookup: want a refusal, got %+v", d)
	}
}
