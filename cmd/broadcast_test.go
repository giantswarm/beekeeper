package cmd

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestBroadcastSkipsRelievedRosterRun(t *testing.T) {
	from := state.Party{Session: "relieved-run", Name: "Guide run 1"}
	to := state.Party{Session: "current-run", Name: "Guide run 2"}
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	st := &state.State{Guide: &state.Role{Holder: &state.Supervisor{Party: from}}}
	st.Agents = []state.Agent{{Party: from}, {Party: to}}
	if _, _, err := guideRole.relay(st, from, to, now, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, _, err := guideRole.start(st, to, true, false, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, ag := range st.Agents {
		if ag.Is(from) {
			t.Fatal("relay left the relieved run on the roster")
		}
	}
	// Simulate a stale roster entry whose CLI still runs after the relay.
	st.Agents = append(st.Agents, state.Agent{Party: from})
	st.Agents = append(st.Agents,
		state.Agent{Party: state.Party{Session: "busy-run", Name: "Busy"}, Task: "broadcast fixture task"},
		state.Agent{Party: state.Party{Session: "idle-run", Name: "Idle"}},
		state.Agent{Party: state.Party{Session: "parked-run", Name: "Parked"}, Task: "broadcast fixture task", Park: &state.Park{}},
		state.Agent{Party: state.Party{Session: "done-run", Name: "Retired"}, Done: true},
		state.Agent{Party: state.Party{Session: "stopped-run", Name: "Stopped"}},
	)
	var sessions []*claude.Session
	for _, ag := range st.Agents {
		if ag.Name != "Stopped" {
			sessions = append(sessions, &claude.Session{ID: ag.Session, Name: ag.Name})
		}
	}
	var sent []string
	told, missed := broadcast(context.Background(), st, sessions, nil, func(_ context.Context, name string) error {
		sent = append(sent, name)
		return nil
	})
	want := []string{to.Name, "Busy", "Idle"}
	if !reflect.DeepEqual(sent, want) || !reflect.DeepEqual(told, want) || len(missed) != 0 {
		t.Fatalf("sent %v, told %v, missed %v; want %v", sent, told, missed, want)
	}
}
