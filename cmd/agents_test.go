package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

const workerName = "test: worker"

func TestRegisterAgentKeepsNamesUnique(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	stale := state.Agent{Party: state.Party{Session: "a", Name: workerName}, Task: "count lines"}
	running := state.Agent{Party: state.Party{Session: "r", Name: "test: busy worker"}}
	other := state.Agent{Party: state.Party{Session: "o", Name: "test: other"}}
	live := func(p state.Party) bool { return p.Session == "r" || p.Session == "b" }

	// A fresh session under the name of one that no longer runs replaces
	// its entry, also a duplicate an older release left behind.
	st := &state.State{Agents: []state.Agent{stale, running, other, stale}}
	b := state.Party{Session: "b", Name: "Test: Worker"}
	replaced, err := registerAgent(st, b, live, now)
	if err != nil || len(replaced) != 2 || replaced[0].Session != "a" || replaced[0].Task != "count lines" {
		t.Fatalf("replaced = %+v, err = %v", replaced, err)
	}
	if len(st.Agents) != 3 || st.Agents[2].Session != "b" || !st.Agents[2].IdleSince.Equal(now) {
		t.Fatalf("roster = %+v", st.Agents)
	}
	if i, err := findAgent(st, workerName); err != nil || st.Agents[i].Session != "b" {
		t.Errorf("findAgent = %d, %v", i, err)
	}

	// Registering again, under another name, replaces its own entry.
	replaced, err = registerAgent(st, state.Party{Session: "b", Name: "test: renamed"}, live, now)
	if err != nil || len(replaced) != 0 || len(st.Agents) != 3 || st.Agents[2].Name != "test: renamed" {
		t.Fatalf("re-register: replaced = %+v, err = %v, roster = %+v", replaced, err, st.Agents)
	}

	// A name a running session's entry holds is refused.
	before := len(st.Agents)
	_, err = registerAgent(st, state.Party{Session: "c", Name: "test: busy worker"}, live, now)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "running session r") || len(st.Agents) != before {
		t.Errorf("live name: err = %v, roster = %+v", err, st.Agents)
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
