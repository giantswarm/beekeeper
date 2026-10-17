package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

const retaskBrief = "# FIX beekeeper 1355 (opus, builds)\n\nIssue: https://github.com/giantswarm/beekeeper/issues/1355"

// A worker that reported its task done and is handed the next by the
// supervisor's message has its task record back: agents park takes a
// target again, the doctor leaves it on the roster, and the park wakes it
// once the target settles.
func TestRetaskedWorkerParksAgain(t *testing.T) {
	w := parkingWatch(t, nil)
	w.cfg.Agents.AutoResume = true
	woke := stubWakes(t)
	sup := yoursSup
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Supervisor = &state.Supervisor{Party: sup, Since: relayNow}
		reportIdle(&st.Agents[0], relayNow)
		st.Agents[0].Done = true
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := park(t, w, "--on", "#3", "Timo's answer"); err == nil || !strings.Contains(err.Error(), "has no task") {
		t.Fatalf("park after idle --done: %v", err)
	}

	line, err := w.retaskAgent(sup, messagedAgent(nil, agentOne, nil), retaskBrief)
	if err != nil {
		t.Fatal(err)
	}
	if want := `Agent one is busy again with "FIX beekeeper 1355 (opus, builds)"`; !strings.HasPrefix(line, want) {
		t.Errorf("the sender is told %q, want %q…", line, want)
	}
	ag := agentOf(t, w)
	if ag.Task != "FIX beekeeper 1355 (opus, builds)" || ag.Done || !ag.AssignedAt.Equal(w.now.UTC()) {
		t.Fatalf("re-tasked agent %+v", ag)
	}
	st, _ := w.store.Read()
	if chores := planChores(st, nil, nil, func(state.Party) bool { return false }, 0, w.now); len(chores) != 0 {
		t.Errorf("the doctor plans %+v for a re-tasked worker", chores)
	}

	if err := park(t, w, "--on", "#3", "Timo's answer"); err != nil {
		t.Fatalf("park after the re-task: %v", err)
	}
	w.pending(context.Background(), nil)
	answer(t, w)
	for range 3 {
		w.pending(context.Background(), nil)
	}
	if len(*woke) != 1 || !strings.Contains((*woke)[0], "Your park on #3 (Timo's answer) is over") {
		t.Errorf("the park woke %q", *woke)
	}
	if !strings.Contains(w.out.(*bytes.Buffer).String(), `AGENT RESUMABLE "Agent one"`) {
		t.Errorf("watch output:\n%s", w.out)
	}
}

// Only the supervisor's message re-tasks, only an agent that had a task
// and reported it idle, and never one that is busy: a worker that never had
// a task still has none to park.
func TestRetaskIsTheSupervisorsForAnIdleWorker(t *testing.T) {
	w := parkingWatch(t, nil)
	sup := yoursSup
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Supervisor = &state.Supervisor{Party: sup, Since: relayNow}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if line, _ := w.retaskAgent(sup, messagedAgent(nil, agentOne, nil), retaskBrief); line != "" {
		t.Errorf("a busy worker was re-tasked: %q", line)
	}
	if ag := agentOf(t, w); ag.Task != parkTask {
		t.Errorf("the busy task became %q", ag.Task)
	}
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Task, st.Agents[0].LastTask = "", ""
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if line, _ := w.retaskAgent(sup, messagedAgent(nil, agentOne, nil), retaskBrief); line != "" {
		t.Errorf("a worker that never had a task was re-tasked: %q", line)
	}
	if err := park(t, w, "the supervisor's go"); err == nil || !strings.Contains(err.Error(), "has no task") {
		t.Errorf("park without any task: %v", err)
	}
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].LastTask = parkTask
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if line, _ := w.retaskAgent(worker, messagedAgent(nil, agentOne, nil), retaskBrief); line != "" {
		t.Errorf("a worker's message re-tasked: %q", line)
	}
	if line, _ := w.retaskAgent(sup, messagedAgent(nil, "Agent nine", nil), retaskBrief); line != "" {
		t.Errorf("a message to nobody re-tasked: %q", line)
	}
	if line, _ := w.retaskAgent(sup, rosterIndex(state.Party{Name: agentOne}), "\n\ncarry on"); line == "" || agentOf(t, w).Task != parkTask {
		t.Errorf("a message without a first line keeps the last task: %q, task %q", line, agentOf(t, w).Task)
	}
}

// A SendMessage to a CLI's peer socket names the roster agent whose
// running CLI listens there.
func TestMessagedAgentBySocket(t *testing.T) {
	st := &state.State{Agents: []state.Agent{
		{Party: state.Party{Session: "s1", Name: agentOne}},
		{Party: state.Party{Session: "s2", Name: agentTwo}},
	}}
	sessions := []*claude.Session{{ID: "s1", PID: 11}, {ID: "s2", PID: 22}}
	sock := func(pid int) string {
		if pid == 22 {
			return "/run/user/1000/cc-socks/22.sock"
		}
		return ""
	}
	if i := messagedAgent(sessions, "uds:/run/user/1000/cc-socks/22.sock", sock)(st); i != 1 {
		t.Errorf("socket of s2: agent %d", i)
	}
	if i := messagedAgent(sessions, "uds:/run/user/1000/cc-socks/99.sock", sock)(st); i != -1 {
		t.Errorf("a socket no CLI listens on: agent %d", i)
	}
	if i := messagedAgent(sessions, agentOne, sock)(st); i != 0 {
		t.Errorf("by name: agent %d", i)
	}
}
