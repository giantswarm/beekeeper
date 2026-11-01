package cmd

import (
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
)

// offID is a CLI session id no roster entry or role names.
const (
	offID    = "22222222-2222-4222-8222-222222222222"
	offOmp   = "omp_abc"
	offCodex = "codex_xyz"
	offLocal = "local_42"
)

// A wake reaches a roster agent first, else a session off the roster: a
// role's holder by its name or ids, a parked session, or any session by its
// own id; anything else is the roster's refusal.
func TestWakePartyReachesSessionsOffTheRoster(t *testing.T) {
	sup := state.Party{Session: "11111111-1111-4111-8111-111111111111", HostSession: "local_sup", Name: "Supervisor run 77"}
	st := &state.State{
		Guide:  &state.Role{Holder: &state.Supervisor{Party: sup}},
		Agents: []state.Agent{{Party: state.Party{Session: "s1", Name: agentOne}}},
		Parks:  []state.Park{{By: state.Party{HostSession: offOmp, Name: "an omp session"}}},
	}
	a := &app{}
	for q, want := range map[string]state.Party{
		"supervisor run 77": sup,
		"local_sup":         sup,
		offOmp:              {HostSession: offOmp, Name: "an omp session"},
		offCodex:            {HostSession: offCodex, Name: offCodex},
		offLocal:            {HostSession: offLocal, Name: offLocal},
		offID:               {Session: offID, Name: offID},
	} {
		ag, rostered, err := a.wakeParty(st, q)
		if err != nil || rostered || ag.Party != want {
			t.Errorf("wakeParty(%q) = %+v, %v, %v; want %+v off the roster", q, ag.Party, rostered, err, want)
		}
	}
	if ag, rostered, err := a.wakeParty(st, agentOne); err != nil || !rostered || ag.Session != "s1" {
		t.Errorf("a roster agent: %+v, %v, %v", ag.Party, rostered, err)
	}
	if _, _, err := a.wakeParty(st, "nobody"); err == nil {
		t.Error("a name nothing carries was resolved")
	}
}
