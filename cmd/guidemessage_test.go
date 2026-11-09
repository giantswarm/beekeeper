package cmd

import (
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestGuideMessageTargetsAfterRelay(t *testing.T) {
	old := state.Party{Name: "Guide run 1", Session: "old-cli", HostSession: "local_old"}
	current := state.Party{Name: "Guide run 2", Session: "current-cli", HostSession: "local_current"}
	st := &state.State{Guide: &state.Role{Holder: &state.Supervisor{Party: current}, Relieved: []state.Relief{{Party: old}}}}
	// Even a stale roster entry must not make an old guide reachable.
	st.Agents = []state.Agent{{Party: old}, {Party: current}}
	a, _ := stubApp(t)
	for _, q := range []string{"guide", "the guide", "GUIDE"} {
		ag, _, err := a.wakeParty(st, q)
		if err != nil || !ag.Is(current) {
			t.Errorf("wake %q = %v, %v", q, ag, err)
		}
	}
	for _, q := range []string{old.Name, old.Session, old.HostSession} {
		_, _, err := a.wakeParty(st, q)
		if Code(err) != ExitRefused || !strings.Contains(err.Error(), current.Name) {
			t.Errorf("stale %q = %v", q, err)
		}
	}
	// Resolution uses the holder at each send, including a second relay.
	next := state.Party{Name: "Guide run 3", Session: "next-cli"}
	st.Guide.Relieved = append(st.Guide.Relieved, state.Relief{Party: current})
	st.Guide.Holder = &state.Supervisor{Party: next}
	if got, err := guideMessageTarget(st, "the guide"); err != nil || got != wakeID(next) {
		t.Fatalf("next = %q, %v", got, err)
	}
	st.Guide.Holder = nil
	if _, err := guideMessageTarget(st, "guide"); Code(err) != ExitRefused {
		t.Fatalf("unheld guide = %v", err)
	}
}

func TestRelayRefusesRelievedGuideBeforeSending(t *testing.T) {
	a, out := stubApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: state.Party{Name: "Guide run 2", Session: "new"}}, Relieved: []state.Relief{{Party: state.Party{Name: "Guide run 1", Session: "old"}}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.relayInstruction(t.Context(), "old", "stale-socket", "request"); Code(err) != ExitRefused || !strings.Contains(err.Error(), "Guide run 2") {
		t.Fatalf("relay = %v (%s)", err, out.String())
	}
}
