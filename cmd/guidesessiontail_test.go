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

const (
	pastGuideName = "Guide run 15"
	pastGuideCLI  = "guide-15-cli"
	pastGuideHost = "local_guide15"
)

// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
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
		st.Agents = []state.Agent{{Party: nextGuide, Task: guideRole.Duty}}
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
