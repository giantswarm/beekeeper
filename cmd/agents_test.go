package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	workerName = "test: worker"
	staleTask  = "count lines"
)

func TestRegisterAgentKeepsNamesUnique(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	assigned := now.Add(-time.Hour)
	stale := state.Agent{Party: state.Party{Session: "a", Name: workerName}, Task: staleTask, AssignedAt: assigned}
	dup := state.Agent{Party: state.Party{Session: "d", Name: workerName}}
	running := state.Agent{Party: state.Party{Session: "r", Name: "test: busy worker"}}
	other := state.Agent{Party: state.Party{Session: "o", Name: "test: other"}}
	live := func(p state.Party) bool { return p.Session == "r" || p.Session == "b" }

	// A fresh session under the name of one that no longer runs replaces
	// its entry, also a duplicate an older release left behind, and takes
	// over the task the stopped session left unfinished.
	st := &state.State{Agents: []state.Agent{stale, running, other, dup}}
	b := state.Party{Session: "b", Name: "Test: Worker"}
	reg, err := registerAgent(st, b, live, now)
	if err != nil || len(reg.replaced) != 2 || reg.replaced[0].Session != "a" || reg.task != staleTask || !reg.assignedAt.Equal(assigned) {
		t.Fatalf("registration = %+v, err = %v", reg, err)
	}
	if len(st.Agents) != 3 || st.Agents[2].Session != "b" || st.Agents[2].Task != staleTask || !st.Agents[2].AssignedAt.Equal(assigned) {
		t.Fatalf("roster = %+v", st.Agents)
	}
	if i, err := findAgent(st, workerName); err != nil || st.Agents[i].Session != "b" {
		t.Errorf("findAgent = %d, %v", i, err)
	}

	// Registering again, under another name, replaces its own entry and
	// keeps its open task.
	reg, err = registerAgent(st, state.Party{Session: "b", Name: "test: renamed"}, live, now)
	if err != nil || len(reg.replaced) != 0 || reg.task != staleTask || len(st.Agents) != 3 || st.Agents[2].Name != "test: renamed" || st.Agents[2].Task != staleTask {
		t.Fatalf("re-register: registration = %+v, err = %v, roster = %+v", reg, err, st.Agents)
	}

	// A replaced entry without an open task leaves the new one idle.
	st.Agents[2].Task = ""
	reg, err = registerAgent(st, state.Party{Session: "e", Name: "test: other"}, live, now)
	if err != nil || len(reg.replaced) != 1 || reg.task != "" || st.Agents[len(st.Agents)-1].Task != "" || !st.Agents[len(st.Agents)-1].IdleSince.Equal(now) {
		t.Fatalf("idle replace: registration = %+v, err = %v, roster = %+v", reg, err, st.Agents)
	}

	// A name a running session's entry holds is refused.
	before := len(st.Agents)
	_, err = registerAgent(st, state.Party{Session: "c", Name: "test: busy worker"}, live, now)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "running session r") || len(st.Agents) != before {
		t.Errorf("live name: err = %v, roster = %+v", err, st.Agents)
	}

	// Two dropped entries with open tasks are refused: one entry holds one.
	st = &state.State{Agents: []state.Agent{stale, {Party: state.Party{Session: "f", Name: "test: f"}, Task: "sum lines"}}}
	_, err = registerAgent(st, state.Party{Session: "f", Name: workerName}, live, now)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "both hold an open task") || len(st.Agents) != 2 {
		t.Errorf("two tasks: err = %v, roster = %+v", err, st.Agents)
	}
}

func TestFindPartyRefusesADuplicateName(t *testing.T) {
	parties := []state.Party{{Session: "a", Name: workerName}, {Session: "b", Name: workerName}, {Session: "c", Name: "test: worker two"}}
	if _, err := findParty(parties, "Test: Worker", "agent", "agents"); Code(err) != ExitRefused || !strings.Contains(err.Error(), "(a, b)") {
		t.Errorf("duplicate name: err = %v", err)
	}
	for q, want := range map[string]string{"b": "b", "two": "c", "test: worker two": "c"} {
		if i, err := findParty(parties, q, "agent", "agents"); err != nil || parties[i].Session != want {
			t.Errorf("findParty(%q) = %d, %v, want %s", q, i, err, want)
		}
	}
	if _, err := findParty(parties, "worker", "agent", "agents"); Code(err) != ExitRefused {
		t.Errorf("partial name of three: err = %v", err)
	}
}
