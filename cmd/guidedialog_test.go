package cmd

import (
	"encoding/json"
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
)

func TestGuideDialogSelectionVerifiesOnlyForItsNote(t *testing.T) {
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
			q := "Merge https://github.com/o/private/pull/7 at abc1234? Status quo: reviewed. Why: review limit."
			raw, _ := json.Marshal(map[string]any{"hook_event_name": guard.PostToolUseEvent, dialogSessionKey: dialogSession, transcriptToolName: guard.AskTool,
				transcriptInputKey: map[string]any{"questions": []any{map[string]any{"question": q, "options": []any{map[string]any{"label": dialogMergeChoice}, map[string]any{"label": "Rework"}}}}},
				"tool_response":    map[string]any{"answers": map[string]string{q: dialogMergeChoice}}})
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
			v, _, err := a.answerAccount(answerQuery{Note: 1, About: privateThread, Head: verifyHead})
			if err != nil {
				t.Fatal(err)
			}
			if v.Verified != tc.want {
				t.Fatalf("verified=%v want %v: %+v", v.Verified, tc.want, v)
			}
			if tc.want && (!strings.Contains(v.Strength, "question dialog") || v.Channel != "guide_dialog") {
				t.Fatalf("no dialog provenance %+v", v)
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
