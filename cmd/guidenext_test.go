package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

// guideNextApp is noteApp with agentOne as the guide and three open notes
// for Pat: #1 undated, #2 due in 2h, #3 due in 1h; #4 is pinned, #5 for
// someone else.
func guideNextApp(t *testing.T) *app {
	t.Helper()
	a, _ := noteApp(t)
	by := state.Party{Name: "Agent nine"}
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: state.Party{Name: agentOne}}}
		st.Notes = []state.Note{
			{ID: 1, For: notePerson, Text: "pick a lab?", By: by},
			{ID: 2, For: notePerson, Text: "approve the ADR?", By: by, Due: relayNow.Add(2 * time.Hour)},
			{ID: 3, For: notePerson, Text: "merge the bump?", By: by, Due: relayNow.Add(time.Hour)},
			{ID: 4, For: notePerson, Text: "standing rule", By: by, Pinned: true},
			{ID: 5, For: "Dana", Text: "the lease?", By: by},
		}
		st.NextNote = 5
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// serveNext runs guide next for a and returns the served note's id, 0 for
// none, and its lines.
func serveNext(t *testing.T, a *app) (int, string, error) {
	t.Helper()
	var it *queueItem
	var lines []string
	me, _ := a.caller()
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		var evs []state.Event
		var err error
		it, lines, evs, err = a.guideNext(st, nil, me)
		return evs, err
	})
	if it == nil {
		return 0, strings.Join(lines, "\n"), err
	}
	return it.Note.ID, strings.Join(lines, "\n"), err
}

func noteCmdRun(t *testing.T, a *app, args ...string) {
	t.Helper()
	c := a.noteCmd()
	c.SetArgs(args)
	c.SetOut(a.out)
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err != nil {
		t.Fatalf("note %v: %v", args, err)
	}
}

func TestGuideNextServesOneDecisionAtATime(t *testing.T) {
	a := guideNextApp(t)
	id, out, err := serveNext(t, a)
	if err != nil || id != 3 || !strings.HasPrefix(out, `asking #3 for Pat from "Agent nine", due `) || !strings.Contains(out, "beekeeper note answer 3") {
		t.Fatalf("first: want #3, due first, got %d %q %v", id, out, err)
	}
	if id, out, _ = serveNext(t, a); id != 3 || !strings.HasPrefix(out, "still asking #3") {
		t.Fatalf("before the answer: want #3 again, got %d %q", id, out)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "guide.asking" })
	if err != nil || len(evs) != 1 {
		t.Fatalf("guide.asking events: want one, got %v %v", evs, err)
	}
	noteCmdRun(t, a, "answer", "3", "yes, merge it")
	if id, _, _ = serveNext(t, a); id != 2 {
		t.Fatalf("after the answer: want #2, got %d", id)
	}
	noteCmdRun(t, a, "done", "2")
	if id, _, _ = serveNext(t, a); id != 1 {
		t.Fatalf("after done: want the undated #1, got %d", id)
	}
	noteCmdRun(t, a, "answer", "1", "agentlab-2")
	id, out, _ = serveNext(t, a)
	if id != 0 || out != "no open decision for Pat" {
		t.Fatalf("with no decision left: got %d %q", id, out)
	}
	st, _ := a.store.Read()
	if st.GuideRole().Asking != 0 {
		t.Fatalf("nothing asked, the mark stays: %d", st.GuideRole().Asking)
	}
}

func TestGuideNextOnlyForTheGuide(t *testing.T) {
	a := guideNextApp(t)
	a.as = "Agent two"
	if _, _, err := serveNext(t, a); Code(err) != ExitRefused {
		t.Fatalf("another session served a decision: %v", err)
	}
}

func TestGuideQuestionGetsTheNoteChecks(t *testing.T) {
	h := guard.Hook{CheckQuestion: checkQuestion, Guide: func(string) (bool, string) { return true, notePerson }}
	ask := func(question string) string {
		raw := `{"tool_name":"AskUserQuestion","session_id":"g","tool_input":{"questions":[{"question":` + jsonString(question) +
			`,"options":[{"label":"merge","description":"it ships today"},{"label":"wait","description":"it ships next week"}]}]}}`
		var o struct {
			D struct{ PermissionDecision, PermissionDecisionReason string } `json:"hookSpecificOutput"`
		}
		if out := h.Decide([]byte(raw)); out != nil {
			_ = json.Unmarshal(out, &o)
		}
		return o.D.PermissionDecision + " " + o.D.PermissionDecisionReason
	}
	if out := ask("Merge " + prURL + "? Status quo: " + sqNow + ". Why: " + whyNow); out != " " {
		t.Fatalf("a complete question is refused: %s", out)
	}
	for name, c := range map[string]struct{ q, want string }{
		"bare #N":       {"Merge #7? Status quo: " + sqNow + ". Why: " + whyNow, "#7 without its full URL"},
		"no status quo": {"Merge " + prURL + "? Why: " + whyNow, "--status-quo \""},
		"no why":        {"Merge " + prURL + "? Status quo: " + sqNow, "--why \""},
		"unchecked":     {"Merge " + prURL + "? Status quo: CI is green. Why: " + whyNow, `"green" without --checked`},
	} {
		if out := ask(c.q); !strings.HasPrefix(out, "deny ") || !strings.Contains(out, c.want) {
			t.Errorf("%s: want a refusal naming %q, got %s", name, c.want, out)
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
