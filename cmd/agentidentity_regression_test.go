package cmd

import (
	"reflect"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestRecordStartRefusesAnotherLiveHostSessionWithTheSameName(t *testing.T) {
	for _, prefix := range []string{"codex_", "agy_", "omp_"} {
		t.Run(prefix, func(t *testing.T) {
			now := time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC)
			original := state.Agent{
				Party: state.Party{HostSession: prefix + "first", Name: workerName},
				Task:  staleTask, AssignedAt: now.Add(-time.Hour),
			}
			st := &state.State{Agents: []state.Agent{original}}
			s := state.Start{Party: state.Party{HostSession: prefix + "second", Name: workerName}, At: now}
			live := func(p state.Party) bool { return p.HostSession == original.HostSession }
			if _, err := recordStart(st, s, "a different task", "", live); Code(err) != ExitRefused {
				t.Fatalf("a second worker took the running worker's name: %v", err)
			}
			if len(st.Starts) != 0 || len(st.Agents) != 1 || !reflect.DeepEqual(st.Agents[0], original) {
				t.Fatalf("a refused start changed the roster or starts: %+v", st)
			}
		})
	}
}

func TestHostSessionCallerCannotActAsSameNamedSuccessor(t *testing.T) {
	const name = "replacement worker"
	st := &state.State{Agents: []state.Agent{{Party: state.Party{HostSession: "codex_successor", Name: name}}}}
	a := &app{}
	if i := a.callerAgent(st, state.Party{HostSession: "codex_predecessor", Name: name}); i != -1 {
		t.Fatalf("the predecessor can act on successor roster entry %d", i)
	}
	// Explicit --as remains a name-based operator action.
	a.as = name
	if i := a.callerAgent(st, state.Party{Name: name}); i != 0 {
		t.Fatalf("explicit --as no longer addresses the roster entry: %d", i)
	}
}
