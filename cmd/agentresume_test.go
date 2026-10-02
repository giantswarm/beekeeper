package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// waitWorker is the session of a worker whose headless turn ends on a wait.
const waitWorker = "29900000-0000-4000-8000-000000000299"

// backgroundTurn is a transcript whose turn launched cmd in the background
// and ended before its completion notice.
func backgroundTurn(cmd string) string {
	return `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"` + cmd + `","description":"Wait for CI","run_in_background":true}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"Command running in background with ID: b1."}]},"toolUseResult":{"backgroundTaskId":"b1"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"The notification will wake me."}]}}
`
}

// waitApp is an app whose roster holds the started worker waitWorker with a
// task, its transcript and serve record as given; resumes collects the
// messages it is resumed with, and the reopen sees no leftover processes and
// no running desktop.
func waitApp(t *testing.T, transcript, waits string) (*app, *[]string) {
	t.Helper()
	a, _ := stubApp(t)
	a.cfg.Desktop.TypingQuiet.Duration = -1
	a.cfg.Guide.Person = "Timo"
	dir := filepath.Join(a.cfg.Claude.ProjectsDir, "-work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, waitWorker+".jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	p := state.Party{Session: waitWorker, HostSession: "local_" + waitWorker, Name: "BK 299"}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{Party: p, Mode: state.ModeBypass, Dir: dir, At: time.Now()})
		st.Agents = append(st.Agents, state.Agent{Party: p, Task: "giantswarm/beekeeper#299", AssignedAt: time.Now().Add(-time.Minute)})
		if waits != "" {
			st.Records = append(st.Records, state.Record{Session: p, Issue: "giantswarm/beekeeper#299", Waits: waits})
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	var resumes []string
	savedResume, savedLeft := resumeHeadless, unitLeftovers
	t.Cleanup(func() { resumeHeadless, unitLeftovers = savedResume, savedLeft })
	resumeHeadless = func(_ context.Context, _ *app, ag state.Agent, msg string) error {
		if ag.Name != "BK 299" {
			t.Errorf("resumed %q", ag.Name)
		}
		resumes = append(resumes, msg)
		return nil
	}
	unitLeftovers = func(string) [][]string { return nil }
	plat.Machine = tableMachine{plat.Machine}
	return a, &resumes
}

// reopen runs the reopen after the worker's headless turn.
func reopen(t *testing.T, a *app) {
	t.Helper()
	c := a.agentReopenCmd()
	c.SetArgs([]string{waitWorker})
	if err := c.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// A headless turn that ended on a background wait with its task open is
// resumed headless once, with the wait named, and logged; the next end of
// the same task's turn is not resumed again.
func TestReopenResumesATurnThatEndedOnAWait(t *testing.T) {
	a, resumes := waitApp(t, backgroundTurn("until gh run view 1 --exit-status; do :; done"), "CI of PR 340")
	reopen(t, a)
	if len(*resumes) != 1 || !strings.Contains((*resumes)[0], `"Wait for CI" ran in the background`) ||
		!strings.Contains((*resumes)[0], "devctl pr wait") {
		t.Fatalf("resumes = %q", *resumes)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if ag := st.Agents[0]; ag.ResumedWait.IsZero() || ag.ResumedOn != `"Wait for CI"` {
		t.Errorf("agent after the resume: %+v", ag)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == resumedWaitEvent })
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, "BK 299") {
		t.Errorf("%s events = %+v, %v", resumedWaitEvent, evs, err)
	}
	reopen(t, a)
	if len(*resumes) != 1 {
		t.Errorf("resumed again for the same task: %q", *resumes)
	}
}

// A process the turn's unit still runs is a wait too, named without its
// arguments' values.
func TestReopenResumesOnALeftoverProcess(t *testing.T) {
	a, resumes := waitApp(t, "{}\n", "")
	unitLeftovers = func(string) [][]string {
		return [][]string{{"/usr/bin/curl", "-H", "Authorization: Bearer s3cr3t", "-o", "model.bin", "https://example.com/model"}}
	}
	reopen(t, a)
	if len(*resumes) != 1 || !strings.Contains((*resumes)[0], `"curl -H -o"`) || strings.Contains((*resumes)[0], "s3cr3t") {
		t.Errorf("resumes = %q", *resumes)
	}
}

// A worker parked on a person, one done, one with no wait open and one whose
// background wait is a devctl command the gate runs (its outcome wakes its
// owner by itself) are left alone.
func TestReopenLeavesAloneWhatNeedsNoResume(t *testing.T) {
	for name, c := range map[string]struct {
		transcript, waits string
		done              bool
	}{
		"parked on the person":   {backgroundTurn("sleep 600"), "Timo's answer on the rollout", false},
		"parked on a note":       {backgroundTurn("sleep 600"), "note #12", false},
		"done":                   {backgroundTurn("sleep 600"), "", true},
		"no wait":                {"{}\n", "CI", false},
		"a gated devctl wait":    {backgroundTurn("devctl pr wait giantswarm/beekeeper 340"), "CI", false},
		"a completed background": {backgroundTurn("make test") + `{"type":"user","message":{"role":"user","content":"<task-notification>\n<task-id>b1</task-id>\n</task-notification>"}}` + "\n", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			a, resumes := waitApp(t, c.transcript, c.waits)
			if c.done {
				if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.Agents[0].Done = true
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			reopen(t, a)
			if len(*resumes) != 0 {
				t.Errorf("resumed: %q", *resumes)
			}
		})
	}
}

// The watch's AGENTS STOPPED line tells an agent parked on a person apart
// from one that ended on a wait again after its resume.
func TestStoppedAgentsTellParkedFromEndedOnAWait(t *testing.T) {
	w, _, out := reportingWatch(t, t.TempDir())
	w.cfg.Guide.Person = "Timo"
	parked, again := staleAgent("Parked"), staleAgent("Again")
	parked.Task, again.Task = "a rollout", "a proof"
	again.AssignedAt = w.now.Add(-time.Hour)
	again.ResumedWait, again.ResumedOn = w.now.Add(-10*time.Minute), `"Wait for CI"`
	st := &state.State{
		Agents:  []state.Agent{parked, again},
		Records: []state.Record{{Session: parked.Party, Waits: "Timo's go for the rollout"}},
	}
	w.stoppedAgents(st, nil)
	s := out.String()
	if !strings.Contains(s, `"Parked" (parked on a person: Timo's go for the rollout`) ||
		!strings.Contains(s, `"Again" (ended on a wait again after its resume at `) || !strings.Contains(s, `on "Wait for CI"`) {
		t.Errorf("stopped line:\n%s", s)
	}
}

func TestParkedOn(t *testing.T) {
	for waits, want := range map[string]bool{
		"Timo's answer":           true,
		"note #4 for the person":  true,
		"the supervisor's go":     true,
		"CI of PR 12":             false,
		"the model download":      false,
		"timothy's branch builds": false,
		"":                        false,
	} {
		if got := parkedOn(waits, "Timo") != ""; got != want {
			t.Errorf("parkedOn(%q) = %v, want %v", waits, got, want)
		}
	}
}

func TestTurnUnit(t *testing.T) {
	for unit, want := range map[string]bool{
		"beekeeper-agent-29900000.service":         true,
		"beekeeper-wake-29900000-0a1b2c3d.service": true,
		"beekeeper-agent-11111111.service":         false,
		"app-claude-1234.scope":                    false,
		"beekeeper-wake-29900000-0a1b2c3d.scope":   false,
	} {
		if got := turnUnit(unit, waitWorker); got != want {
			t.Errorf("turnUnit(%q) = %v, want %v", unit, got, want)
		}
	}
}
