package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	dialogSession       = "guide-session"
	dialogWorkerSession = "worker-session"
	dialogMergeChoice   = "Merge"
	dialogSessionKey    = "session_id"
	// dialogMergeQuestion is the note's dialog question all three dialog
	// fixtures ask, so an answer recorded for it matches everywhere.
	dialogMergeQuestion = "Merge https://github.com/o/private/pull/7 at abc1234? Status quo: reviewed. Why: review limit."
	dialogLabelKey      = "label"
	dialogAnswersKey    = "answers"
	dialogQuestionKey   = "question"
	dialogQuestionsKey  = "questions"
	dialogToolRespKey   = "tool_response"
	// pastGuideName names the ended guide run the successor test tails,
	// pastGuideCLI its CLI session and pastGuideHost its desktop host session.
	pastGuideName = "Guide run 15"
	pastGuideCLI  = "guide-15-cli"
	pastGuideHost = "local_guide15"
)

func TestGuideDialogSelectionIsAttributedOnlyForItsNote(t *testing.T) {
	for _, tc := range []struct {
		name, session, words string
		choice               int
		want                 bool
	}{
		{"selection", dialogSession, "", 1, true},
		{"verbatim", dialogSession, dialogMergeChoice, 1, true},
		{"different choice", dialogSession, "", 2, false},
		{"invented words", dialogSession, "merge anything", 1, false},
		{"worker relay", dialogWorkerSession, "", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := answerApp(t)
			a.now = time.Now().Add(time.Second)
			stubWakes(t)
			stubSeat(t, tc.session, false)
			verifyNote(t, a)
			guide := state.Party{Session: dialogSession, Name: verifyGuide}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Notes[0].Options = []string{"Merge: merge this head", "Rework: fix the branch"}
				st.Notes[0].At = time.Now().Add(-time.Minute)
				st.SetGuideRole(state.Role{Holder: &state.Supervisor{Party: guide}, Asking: 1})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			q := dialogMergeQuestion
			raw, _ := json.Marshal(map[string]any{transcriptHookEventKey: guard.PostToolUseEvent, dialogSessionKey: dialogSession, transcriptToolName: guard.AskTool,
				transcriptInputKey: map[string]any{dialogQuestionsKey: []any{map[string]any{dialogQuestionKey: q, paramOptions: []any{map[string]any{dialogLabelKey: dialogMergeChoice}, map[string]any{dialogLabelKey: "Rework"}}}}},
				dialogToolRespKey:  map[string]any{dialogAnswersKey: map[string]string{q: dialogMergeChoice}}})
			a.redactResult(raw)
			// Use the real session identity, not a caller-supplied role name.
			a.as = ""
			t.Setenv("BEEKEEPER_OMP_AGENT", "")
			t.Setenv("BEEKEEPER_AGY_AGENT", "")
			t.Setenv("BEEKEEPER_CODEX_AGENT", "")
			t.Setenv("CLAUDE_CODE_SESSION_ID", tc.session)
			t.Setenv("CLAUDE_CODE_SESSION_NAME", verifyGuide)
			if err := a.recordAnswer(t.Context(), privateThread, 1, tc.choice, viaCLI, tc.words, false); err != nil {
				t.Fatal(err)
			}
			got, err := a.answeredSince(a.now.Add(-time.Hour))
			if err != nil || len(got) != 1 {
				t.Fatalf("answers: %+v, %v", got, err)
			}
			if (got[0].Via == channelGuideDialog) != tc.want {
				t.Fatalf("dialog attribution: %+v", got[0])
			}
		})
	}
}

func TestGuideDialogIgnoresCancelledStaleAndForeignResults(t *testing.T) {
	a, _ := answerApp(t)
	a.now = time.Now()
	verifyNote(t, a)
	guide := state.Party{Session: dialogSession, Name: verifyGuide}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.SetGuideRole(state.Role{Holder: &state.Supervisor{Party: guide}, Asking: 1})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"hook_event_name":"PostToolUse","session_id":"worker","tool_name":"AskUserQuestion","tool_response":{"answers":{"Merge?":"yes"}}}`,
		`{"hook_event_name":"PostToolUse","session_id":"guide-session","tool_name":"AskUserQuestion","tool_response":{"answers":{}}}`,
		`{"hook_event_name":"PreToolUse","session_id":"guide-session","tool_name":"AskUserQuestion"}`,
	} {
		a.redactResult([]byte(raw))
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "guide.dialog" })
	if err != nil || len(evs) != 0 {
		t.Fatalf("unexpected receipts %+v %v", evs, err)
	}
}

func TestGuideDialogKeepsSelectedOptionAndNotesVerbatim(t *testing.T) {
	const (
		opt1Label = "Approach 1"
		opt1Full  = "Approach 1: use a dedicated service account"
		opt2Label = "Approach 2"
		opt2Full  = "Approach 2: reuse the existing token"
		userNotes = "keep the existing secret name and document the migration"
		wantFull  = opt1Label + " — " + userNotes
	)
	for _, tc := range []struct {
		name   string
		choice int
		words  string
	}{
		{"choice and notes", 1, userNotes},
		{"choice and full text", 1, wantFull},
		{"choice and full option with description", 1, opt1Full + " — " + userNotes},
		{"choice and recommended prefix", 1, opt1Label + " (Recommended) — " + userNotes},
		{"verbatim full text without choice flag", 0, wantFull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := answerApp(t)
			a.now = time.Now().Add(time.Second)
			woke := stubWakes(t)
			stubSeat(t, dialogSession, false)
			verifyNote(t, a)
			guide := state.Party{Session: dialogSession, Name: verifyGuide}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Notes[0].Options = []string{opt1Full, opt2Full}
				st.Notes[0].At = time.Now().Add(-time.Minute)
				st.SetGuideRole(state.Role{Holder: &state.Supervisor{Party: guide}, Asking: 1})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			q := dialogMergeQuestion
			raw, _ := json.Marshal(map[string]any{
				transcriptHookEventKey: guard.PostToolUseEvent,
				dialogSessionKey:       dialogSession,
				transcriptToolName:     guard.AskTool,
				transcriptInputKey:     map[string]any{dialogQuestionsKey: []any{map[string]any{dialogQuestionKey: q, paramOptions: []any{map[string]any{dialogLabelKey: opt1Label + " (Recommended)"}, map[string]any{dialogLabelKey: opt2Label}}}}},
				dialogToolRespKey:      map[string]any{dialogAnswersKey: map[string]string{q: opt1Label + " (Recommended)"}, "annotations": map[string]any{q: map[string]string{keyNotes: userNotes}}},
			})
			a.redactResult(raw)
			a.as = ""
			t.Setenv("BEEKEEPER_OMP_AGENT", "")
			t.Setenv("BEEKEEPER_AGY_AGENT", "")
			t.Setenv("BEEKEEPER_CODEX_AGENT", "")
			t.Setenv("CLAUDE_CODE_SESSION_ID", dialogSession)
			t.Setenv("CLAUDE_CODE_SESSION_NAME", verifyGuide)
			if err := a.recordAnswer(t.Context(), privateThread, 1, tc.choice, viaCLI, tc.words, false); err != nil {
				t.Fatalf("recordAnswer: %v", err)
			}
			got, err := a.answeredSince(time.Time{})
			if err != nil || len(got) != 1 {
				t.Fatalf("answeredSince = %+v, %v", got, err)
			}
			if got[0].Answer != wantFull {
				t.Fatalf("answer.recorded = %q, want %q", got[0].Answer, wantFull)
			}
			if len(*woke) != 1 || !strings.Contains((*woke)[0], ": "+wantFull+" (on o/private#7)") {
				t.Fatalf("woke = %q, want %q", *woke, wantFull)
			}
		})
	}
}

func TestGuideDialogRefusesOptionLabelOnlyWhenPersonWroteText(t *testing.T) {
	const (
		opt1Label = "Approach 1"
		opt1Full  = "Approach 1: use a dedicated service account"
		opt2Label = "Approach 2"
		opt2Full  = "Approach 2: reuse the existing token"
		userNotes = "also rotate the token weekly"
	)
	for _, tc := range []struct {
		name           string
		fromTranscript bool
		noteID         int
		choice         int
		words          string
	}{
		{"choice with empty words when dialog has notes", false, 1, 1, ""},
		{"choice with option label only when dialog has notes", false, 1, 1, opt1Label},
		{"verbatim option label only when dialog has notes", false, 1, 0, opt1Full},
		{"recommended option label only when dialog has notes", false, 1, 0, opt1Label + " (Recommended)"},
		{"omitted note flag when asking note and dialog has notes", false, 0, 0, opt1Label},
		{"choice only when the transcript's dialog for this note has notes", true, 1, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := answerApp(t)
			now := time.Date(2026, 10, 30, 12, 0, 10, 0, time.UTC)
			a.now = now
			woke := stubWakes(t)
			stubSeat(t, dialogSession, false)
			verifyNote(t, a)
			guide := state.Party{Session: dialogSession, Name: verifyGuide}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Notes[0].Options = []string{opt1Full, opt2Full}
				st.Notes[0].At = now.Add(-time.Minute)
				st.SetGuideRole(state.Role{Holder: &state.Supervisor{Party: guide, Since: now.Add(-30 * time.Second)}, Asking: 1})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			q := dialogMergeQuestion
			resp := map[string]any{dialogAnswersKey: map[string]string{q: opt1Label}}
			if !tc.fromTranscript {
				resp["annotations"] = map[string]any{q: map[string]string{keyNotes: userNotes}}
			} else {
				a.cfg.Claude.ProjectsDir = t.TempDir()
				projDir := filepath.Join(a.cfg.Claude.ProjectsDir, "proj")
				if err := os.MkdirAll(projDir, 0o700); err != nil {
					t.Fatal(err)
				}
				// No hook receipt: the note's own dialog in the transcript,
				// bound by its thread URL, carries the notes.
				writeDialogTranscript(t, filepath.Join(projDir, dialogSession+".jsonl"), q, opt1Label, userNotes)
			}
			raw, _ := json.Marshal(map[string]any{
				transcriptHookEventKey: guard.PostToolUseEvent,
				dialogSessionKey:       dialogSession,
				transcriptToolName:     guard.AskTool,
				transcriptInputKey:     map[string]any{dialogQuestionsKey: []any{map[string]any{dialogQuestionKey: q, paramOptions: []any{map[string]any{dialogLabelKey: opt1Label}, map[string]any{dialogLabelKey: opt2Label}}}}},
				dialogToolRespKey:      resp,
			})
			if !tc.fromTranscript {
				a.redactResult(raw)
			}
			a.as = ""
			t.Setenv("BEEKEEPER_OMP_AGENT", "")
			t.Setenv("BEEKEEPER_AGY_AGENT", "")
			t.Setenv("BEEKEEPER_CODEX_AGENT", "")
			t.Setenv("CLAUDE_CODE_SESSION_ID", dialogSession)
			t.Setenv("CLAUDE_CODE_SESSION_NAME", verifyGuide)
			err := a.recordAnswer(t.Context(), privateThread, tc.noteID, tc.choice, viaCLI, tc.words, false)
			if Code(err) != ExitRefused || !strings.Contains(err.Error(), userNotes) {
				t.Fatalf("recordAnswer err = %v (code %d), want ExitRefused mentioning %q", err, Code(err), userNotes)
			}
			if len(*woke) != 0 {
				t.Fatalf("unexpected wake %q", *woke)
			}
			st, _ := a.store.Read()
			if len(st.Notes) != 1 {
				t.Fatalf("note should remain open: %+v", st.Notes)
			}
		})
	}
}

// writeDialogTranscript writes a guide transcript holding an unrelated
// earlier user turn and an AskUserQuestion call for question answered with
// label and notes (no notes: label alone).
func writeDialogTranscript(t *testing.T, path, question, label, notes string) {
	t.Helper()
	in, _ := json.Marshal(map[string]any{dialogQuestionsKey: []any{map[string]any{dialogQuestionKey: question}}})
	res := map[string]any{dialogAnswersKey: map[string]string{question: label}}
	if notes != "" {
		res["annotations"] = map[string]any{question: map[string]string{keyNotes: notes}}
	}
	out, _ := json.Marshal(res)
	lines := []string{
		`{"type":"user","timestamp":"2026-10-30T12:00:01Z","message":{"role":"user","content":"ok, let's go through the queue"}}`,
		`{"type":"assistant","timestamp":"2026-10-30T12:00:04Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"ask1","name":"AskUserQuestion","input":` + string(in) + `}]}}`,
		`{"type":"user","timestamp":"2026-10-30T12:00:05Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"ask1","content":"answered"}]},"toolUseResult":` + string(out) + `}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A label-only answer is the person's whole answer when their dialog for the
// note carried no notes: other things they said in the guide session, before
// or after, are not words for this note.
func TestGuideDialogAcceptsOptionLabelOnlyBesideUnrelatedTurns(t *testing.T) {
	const (
		opt1Label = "Approach 1"
		opt1Full  = "Approach 1: use a dedicated service account"
		opt2Full  = "Approach 2: reuse the existing token"
	)
	for _, tc := range []struct {
		name    string
		receipt bool
		// question is the transcript dialog's question, notes its notes.
		question, notes string
	}{
		{"label-only dialog receipt, unrelated earlier turn", true, dialogMergeQuestion, ""},
		{"label-only dialog receipt, notes on another note's dialog", true, "Which approach for the other thread?", "something else entirely"},
		{"no receipt, label-only dialog for this note in the transcript", false, dialogMergeQuestion, ""},
		{"no receipt, notes only on another note's dialog", false, "Which approach for the other thread?", "something else entirely"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := answerApp(t)
			now := time.Date(2026, 10, 30, 12, 0, 10, 0, time.UTC)
			a.now = now
			stubWakes(t)
			stubSeat(t, dialogSession, false)
			verifyNote(t, a)
			guide := state.Party{Session: dialogSession, Name: verifyGuide}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Notes[0].Options = []string{opt1Full, opt2Full}
				st.Notes[0].At = now.Add(-time.Hour)
				st.SetGuideRole(state.Role{Holder: &state.Supervisor{Party: guide, Since: now.Add(-30 * time.Second)}, Asking: 1})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			a.cfg.Claude.ProjectsDir = t.TempDir()
			projDir := filepath.Join(a.cfg.Claude.ProjectsDir, "proj")
			if err := os.MkdirAll(projDir, 0o700); err != nil {
				t.Fatal(err)
			}
			writeDialogTranscript(t, filepath.Join(projDir, dialogSession+".jsonl"), tc.question, opt1Label, tc.notes)
			if tc.receipt {
				q := dialogMergeQuestion
				raw, _ := json.Marshal(map[string]any{
					transcriptHookEventKey: guard.PostToolUseEvent,
					dialogSessionKey:       dialogSession,
					transcriptToolName:     guard.AskTool,
					transcriptInputKey:     map[string]any{dialogQuestionsKey: []any{map[string]any{dialogQuestionKey: q, paramOptions: []any{map[string]any{dialogLabelKey: opt1Label}, map[string]any{dialogLabelKey: "Approach 2"}}}}},
					dialogToolRespKey:      map[string]any{dialogAnswersKey: map[string]string{q: opt1Label}},
				})
				a.redactResult(raw)
			}
			a.as = ""
			t.Setenv("BEEKEEPER_OMP_AGENT", "")
			t.Setenv("BEEKEEPER_AGY_AGENT", "")
			t.Setenv("BEEKEEPER_CODEX_AGENT", "")
			t.Setenv("CLAUDE_CODE_SESSION_ID", dialogSession)
			t.Setenv("CLAUDE_CODE_SESSION_NAME", verifyGuide)
			if err := a.recordAnswer(t.Context(), privateThread, 1, 1, viaCLI, "", false); err != nil {
				t.Fatalf("recordAnswer: %v", err)
			}
			got, err := a.answeredSince(time.Time{})
			if err != nil || len(got) != 1 || got[0].Answer != opt1Label {
				t.Fatalf("answeredSince = %+v, %v; want %q", got, err, opt1Label)
			}
		})
	}
}

func TestSuccessorGuideReadsPersonsWordsFromPastGuideSession(t *testing.T) {
	a, _ := answerApp(t)
	now := time.Date(2026, 10, 30, 14, 0, 0, 0, time.UTC)
	a.now = now
	a.cfg.Claude.ProjectsDir = t.TempDir()
	a.cfg.Claude.DesktopDir = t.TempDir()
	pastGuide := state.Party{Session: pastGuideCLI, HostSession: pastGuideHost, Name: pastGuideName}
	nextGuide := state.Party{Session: "guide-16-cli", HostSession: "local_guide16", Name: "Guide run 16"}

	projDir := filepath.Join(a.cfg.Claude.ProjectsDir, "proj")
	if err := os.MkdirAll(projDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(projDir, pastGuideCLI+".jsonl")
	lines := []string{
		`{"type":"assistant","timestamp":"2026-10-30T13:55:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{"questions":[{"question":"Which approach?"}]}}]}}`,
		`{"type":"user","timestamp":"2026-10-30T13:55:10Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"q1","content":"User has answered your questions."}]},"toolUseResult":{"answers":{"Which approach?":"Approach 1"},"annotations":{"Which approach?":{"notes":"keep the existing config path and do not rename the flag"}}}}`,
		`{"type":"user","timestamp":"2026-10-30T13:56:00Z","message":{"role":"user","content":"Also make sure the migration is backwards compatible."}}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Archived after Guide run 16 took over.
	recDir := filepath.Join(a.cfg.Claude.DesktopDir, "acct", "org")
	if err := os.MkdirAll(recDir, 0o700); err != nil {
		t.Fatal(err)
	}
	recJSON, _ := json.Marshal(map[string]any{
		"sessionId":      pastGuideHost,
		"cliSessionId":   pastGuideCLI,
		"title":          pastGuideName,
		"isArchived":     true,
		"lastActivityAt": now.Add(-4 * time.Minute).UnixMilli(),
	})
	if err := os.WriteFile(filepath.Join(recDir, "local_guide15.json"), recJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		// Guide run 16 replaced Guide run 15 (so Guide run 15 is no longer in st.Agents).
		st.Agents = []state.Agent{{Party: nextGuide, Task: guideRole.duty}}
		st.SetGuideRole(state.Role{Holder: &state.Supervisor{Party: nextGuide, Since: now}})
		return []state.Event{
			{At: now.Add(-10 * time.Minute), By: pastGuide, Verb: guideStartVerb, Detail: "as " + pastGuideName},
			{At: now, By: nextGuide, Verb: guideStartVerb, Detail: "as Guide run 16"},
		}, nil
	}); err != nil {
		t.Fatal(err)
	}

	// sessionTail directly, as rostername_test.go does: the tail command
	// itself starts with the process table, which the darwin and
	// systemd-less Linux builds do not have.
	for _, target := range []string{pastGuideName, pastGuideCLI, pastGuideHost} {
		turns, err := a.sessionTail(nil, target, 5, false, now)
		if err != nil {
			t.Fatalf("tail %q: %v", target, err)
		}
		var sb strings.Builder
		for _, tr := range turns {
			sb.WriteString(tr.Text + "\n")
		}
		got := sb.String()
		for _, want := range []string{
			"Approach 1 — keep the existing config path and do not rename the flag",
			"Also make sure the migration is backwards compatible.",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("tail %q turns lack %q:\n%s", target, want, got)
			}
		}
	}

	// An agent on the roster with no running CLI and no transcript still reports no CLI runs.
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: state.Party{Session: "gone-worker", Name: "Gone worker"}})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.sessionTail(nil, "Gone worker", 3, false, now); Code(err) != ExitRefused || !strings.Contains(err.Error(), `no CLI of "Gone worker" runs`) {
		t.Fatalf("tail Gone worker = %v", err)
	}
}
