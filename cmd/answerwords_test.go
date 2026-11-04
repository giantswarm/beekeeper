package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestAnswerKeepsPersonsExtraWordsWithoutProvenance(t *testing.T) {
	for _, notes := range []string{"", "keep the existing configuration"} {
		t.Run(notes, func(t *testing.T) {
			a, _ := answerApp(t)
			a.cfg.Claude.ProjectsDir = t.TempDir()
			guide := state.Party{Session: "answer-words-guide", Name: mailGuide}
			question := "Which approach for https://github.com/o/private/pull/7?"
			result, _ := json.Marshal(map[string]any{
				"answers":     map[string]string{question: "Approach 1"},
				"annotations": map[string]any{question: map[string]string{"notes": notes}},
			})
			lines := []string{
				`{"type":"assistant","timestamp":"2026-10-30T12:00:04Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"ask1","name":"AskUserQuestion","input":{"questions":[{"question":"` + question + `"}]}}]}}`,
				`{"type":"user","timestamp":"2026-10-30T12:00:05Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"ask1","content":"answered"}]},"toolUseResult":` + string(result) + `}`,
			}
			project := filepath.Join(a.cfg.Claude.ProjectsDir, "fixture")
			if err := os.MkdirAll(project, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(project, guide.Session+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			n := state.Note{ID: 1, At: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), Refs: []string{privateThread}, Options: []string{"Approach 1: preserve configuration"}}
			st := &state.State{}
			st.SetGuideRole(state.Role{Holder: &state.Supervisor{Party: guide}, Asking: 1})
			err := a.refuseOptionLabelOnly(st, guide, &n, 1, "")
			if notes == "" && err != nil {
				t.Fatalf("label-only answer refused: %v", err)
			}
			if notes != "" && (err == nil || !strings.Contains(err.Error(), notes)) {
				t.Fatalf("extra words lost: %v", err)
			}
		})
	}
}
