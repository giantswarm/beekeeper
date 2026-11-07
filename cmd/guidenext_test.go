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
// for Pat: #1 undated, #2 due in 2h, #3 due in 1h; #5 for
// someone else.
func guideNextApp(t *testing.T) *app {
	t.Helper()
	a, _ := noteApp(t)
	by := state.Party{Name: "Agent nine"}
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: state.Party{Name: agentOne}}}
		st.Notes = []state.Note{
			{ID: 1, Kind: noteDecision, For: notePerson, Text: "pick a lab?", By: by, Refs: []string{refOne}},
			{ID: 2, Kind: noteDecision, For: notePerson, Text: "approve the ADR?", By: by, Due: relayNow.Add(2 * time.Hour)},
			{ID: 3, Kind: noteDecision, For: notePerson, Text: "merge the bump?", By: by, Due: relayNow.Add(time.Hour), Refs: []string{"o/r#13"}},
			{ID: 5, Kind: noteDecision, For: "Dana", Text: "the lease?", By: by},
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
		it, lines, evs, err = a.guideNext(st, nil, me, nil)
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
	noteCmdRun(t, a, "done", "2", flagNoteReason, withdrawalReason)
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
	if out := ask("Merge " + prURL + "? Status quo: " + sqNow + ". Why: " + whyNow); !strings.Contains(out, "blocking question dialog") {
		t.Fatalf("a blocking question must be refused: %s", out)
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

func runGuideSubcmd(t *testing.T, a *app, args ...string) string {
	t.Helper()
	var buf strings.Builder
	a.out = &buf
	c := a.guideCmd()
	c.SetArgs(args)
	c.SetOut(&buf)
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err != nil {
		t.Fatalf("guide %v: %v", args, err)
	}
	return buf.String()
}

func TestGuideQueueAndNextKeepDeferredAndTodoItemsInQueue(t *testing.T) {
	a, _ := noteApp(t)
	by := state.Party{Name: "Guide run 27", Session: "guide-27"}
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: state.Party{Name: agentOne}}}
		st.Notes = []state.Note{
			{
				ID:        1,
				Kind:      noteDecision,
				For:       notePerson,
				Text:      "Make agents read-only on https://github.com/o/r/issues/1? Status quo: shared access. Why: access scope is the person's decision. Option: " + optMerge + ". Option: " + optWait + ".",
				Question:  "Make agents read-only on https://github.com/o/r/issues/1?",
				StatusQuo: "shared access",
				Options:   []string{optMerge, optWait},
				Default:   "keep shared access",
				By:        by,
				At:        relayNow,
				Due:       relayNow.Add(24 * time.Hour),
				Refs:      []string{refOne},
			},
			{
				ID:      2,
				Kind:    noteDecision,
				For:     notePerson,
				Text:    "Turn hooks back on for https://github.com/o/r/issues/3370?",
				Default: "keep hooks off until approved",
				By:      by,
				At:      relayNow,
				Due:     relayNow.Add(24 * time.Hour),
				Refs:    []string{"o/r#3370"},
			},
		}
		st.NextNote = 2
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	id, out, err := serveNext(t, a)
	if err != nil || id != 1 || !strings.Contains(out, "--choice <n>") || !strings.Contains(out, "--keep-open / --defer <due>") {
		t.Fatalf("first serveNext: got id=%d out=%q err=%v", id, out, err)
	}

	// Person asks a side question instead of picking Option 1 or 2; guide records it with --defer.
	noteCmdRun(t, a, "answer", "1", "--defer", "2h", "--reason", "ask with the hooks test result", "what happens if we turn hooks back on?")

	// Both answer.recorded and note.deferred must be in the audit log, and note #1 stays open.
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == answerRecorded || e.Verb == noteDeferred })
	if err != nil || len(evs) != 2 || evs[0].Verb != answerRecorded || evs[1].Verb != noteDeferred {
		t.Fatalf("events after deferred answer: %+v %v", evs, err)
	}

	// Add a person-owned todo item (company PR review) without --default or --due.
	noteCmdRun(t, a, "add", "--for", notePerson, "--kind", noteTodo, "Please review https://github.com/o/r/pull/7098")

	// Queue must contain #1 (deferred), #2 (decision), and #3 ([todo]).
	qOut := runGuideSubcmd(t, a, "queue", "--full")
	if strings.Contains(qOut, "nothing waits on the person") {
		t.Fatalf("queue falsely says nothing waits: %s", qOut)
	}
	for _, want := range []string{
		"#1 for Pat",
		"deferred until " + clock(relayNow, relayNow.Add(2*time.Hour)) + " (ask with the hooks test result)",
		"#2 for Pat",
		"#3 [todo] for Pat",
		"https://github.com/o/r/pull/7098",
	} {
		if !strings.Contains(qOut, want) {
			t.Fatalf("queue output missing %q:\n%s", want, qOut)
		}
	}

	// guide next skips deferred #1 and serves #2 now.
	if id, _, err = serveNext(t, a); err != nil || id != 2 {
		t.Fatalf("after deferring #1, want #2 next, got %d (%v)", id, err)
	}
	noteCmdRun(t, a, "answer", "2", "yes, turn hooks back on")

	// Next is #3 (the person todo), which can be answered or closed when done.
	if id, _, err = serveNext(t, a); err != nil || id != 3 {
		t.Fatalf("after #2, want todo #3 next, got %d (%v)", id, err)
	}
	noteCmdRun(t, a, "answer", "3", "reviewed and approved")

	// Now only deferred note #1 remains. Queue must STILL list #1, while guide next
	// distinguishes "no decision currently due" from "no open decision".
	qOut = runGuideSubcmd(t, a, "queue", "--full")
	if strings.Contains(qOut, "nothing waits on the person") || !strings.Contains(qOut, "#1 for Pat") {
		t.Fatalf("queue lost deferred note #1:\n%s", qOut)
	}
	id, out, err = serveNext(t, a)
	if err != nil || id != 0 || !strings.Contains(out, "no decision currently due for Pat; 1 deferred note(s) wait in the queue") {
		t.Fatalf("guide next while #1 is deferred: got id=%d out=%q err=%v", id, out, err)
	}

	// Once the deferral time arrives, guide next serves #1 again and --choice closes it.
	a.now = relayNow.Add(2*time.Hour + time.Minute)
	if id, _, err = serveNext(t, a); err != nil || id != 1 {
		t.Fatalf("after deferral time, want #1 served again, got %d (%v)", id, err)
	}
	noteCmdRun(t, a, "answer", "1", "--choice", "1", "ship today")
	if qOut = runGuideSubcmd(t, a, "queue", "--full"); strings.TrimSpace(qOut) != "nothing waits on the person" {
		t.Fatalf("after all items settled, want empty queue, got:\n%s", qOut)
	}
}

func TestNoteReopenAndHandoverUnbackedProseWarning(t *testing.T) {
	a, _ := noteApp(t)
	deskDir := t.TempDir()
	a.cfg.Guide.Desk = deskDir
	by := state.Party{Name: "Guide run 27", Session: "guide-27"}
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: state.Party{Name: agentOne}}, Run: 27}
		st.Notes = []state.Note{
			{
				ID:        1,
				Kind:      noteDecision,
				For:       notePerson,
				Text:      "Make agents read-only on https://github.com/o/r/issues/1? Status quo: shared access. Why: access scope is the person's decision. Option: " + optMerge + ". Option: " + optWait + ".",
				Question:  "Make agents read-only on https://github.com/o/r/issues/1?",
				StatusQuo: "shared access",
				Options:   []string{optMerge, optWait},
				Default:   "keep shared access",
				By:        by,
				At:        relayNow,
				Due:       relayNow.Add(24 * time.Hour),
				Refs:      []string{refOne},
			},
		}
		st.NextNote = 1
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the incident: note #1 was closed by a plain `note answer` without --choice,
	// and the guide wrote prose items under "## Open with Pat" in handover-guide-run-27.md.
	noteCmdRun(t, a, "answer", "1", "what happens if we turn hooks back on?")
	writeFile(t, deskDir+"/notes/handover-guide-run-27.md", strings.Join([]string{
		"# Hand-over — Guide run 27",
		"",
		"## Open with Pat",
		"- Note 1 access scope question unanswered (Pat asked what turning hooks back on does). Ask with the hooks test result.",
		"- Company review PR https://github.com/o/r/pull/7098 needs Pat's review.",
	}, "\n"))

	// Both ordinary handover and --prompt must flag the unbacked prose items.
	hOut := runGuideSubcmd(t, a, "handover", "--full")
	for _, want := range []string{
		"## Unqueued open-with-person items (warning)",
		"Prose notes are not tracked by `beekeeper guide queue`",
		"mentions note #1 as open with the person, but note #1 is not in the open queue",
		"mentions o/r#7098 as open with the person with no matching open note in the queue",
	} {
		if !strings.Contains(hOut, want) {
			t.Fatalf("handover missing %q:\n%s", want, hOut)
		}
	}
	pOut := runGuideSubcmd(t, a, "handover", "--prompt")
	if !strings.Contains(pOut, "Unqueued open-with-person items (warning)") || !strings.Contains(pOut, "note #1") || !strings.Contains(pOut, "o/r#7098") {
		t.Fatalf("handover --prompt missing unbacked warning:\n%s", pOut)
	}

	// Reopen note #1 and add a todo note for o/r#7098; prior answer.recorded history stays intact.
	noteCmdRun(t, a, "reopen", "1", "--due", "3h", "--reason", "access option still undecided")
	noteCmdRun(t, a, "add", "--for", notePerson, "--kind", noteTodo, "Please review https://github.com/o/r/pull/7098")

	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == answerRecorded || e.Verb == noteReopened })
	if err != nil || len(evs) != 2 || evs[0].Verb != answerRecorded || evs[1].Verb != noteReopened {
		t.Fatalf("expected answer.recorded + note.reopened in audit log, got: %+v (%v)", evs, err)
	}

	// Now both items are backed in the queue, so handover has no unbacked warning.
	hOut = runGuideSubcmd(t, a, "handover", "--full")
	if strings.Contains(hOut, "Unqueued open-with-person items") {
		t.Fatalf("unexpected unbacked warning after reopening/adding notes:\n%s", hOut)
	}

	// Reopened note #1 retains its options and refs, so answering with --choice 1 succeeds.
	noteCmdRun(t, a, "answer", "1", "--choice", "1", "merge")
}

func TestKeptOpenReplyRetainsWordsAndRenewsExpiredDecision(t *testing.T) {
	a := guideNextApp(t)
	words := "Ship (Recommended) — can we discuss this first?"
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = []state.Note{{ID: 1, Kind: noteDecision, For: notePerson, By: state.Party{Name: "Agent nine"}, Text: "Ship?", Options: []string{"Ship: release today", "Wait: release later"}, Refs: []string{refOne}, Due: a.now.Add(-time.Hour)}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	noteCmdRun(t, a, "answer", "1", "--keep-open", words)
	evs, err := a.store.Events(1, func(e state.Event) bool { return e.Verb == answerRecorded })
	if err != nil || len(evs) != 1 {
		t.Fatalf("answer event: %v %v", evs, err)
	}
	answer, ok := parseAnswered(evs[0])
	if !ok || answer.Answer != words {
		t.Fatalf("kept-open reply changed: %+v", answer)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if due := defaultDue(st, a.now); len(due) != 0 || len(st.Notes) != 1 {
		t.Fatalf("kept-open note immediately defaulted: %+v", due)
	}
}

func TestReopenRetainsTodoKindAndFiler(t *testing.T) {
	for _, closeWith := range []string{"answer", "done"} {
		t.Run(closeWith, func(t *testing.T) {
			a, _ := noteApp(t)
			by := state.Party{Name: "Agent nine", Session: "original-filer"}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Notes = []state.Note{{ID: 1, Kind: noteTodo, For: notePerson, By: by, Text: "Review the proposal"}}
				st.NextNote = 1
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			if closeWith == "answer" {
				noteCmdRun(t, a, "answer", "1", "done")
			} else {
				noteCmdRun(t, a, "done", "1", "--reason", "withdrawn")
			}
			noteCmdRun(t, a, "reopen", "1")
			st, err := a.store.Read()
			if err != nil || len(st.Notes) != 1 {
				t.Fatalf("reopened state: %+v %v", st, err)
			}
			n := st.Notes[0]
			if n.Kind != noteTodo || n.By.Session != by.Session || !n.Due.IsZero() {
				t.Fatalf("reopen lost todo metadata: %+v", n)
			}
		})
	}
}
