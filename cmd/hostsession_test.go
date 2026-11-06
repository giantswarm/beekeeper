package cmd

import (
	"reflect"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestHostSessionRosterIdentity(t *testing.T) {
	for _, prefix := range []string{"codex_", "agy_", "omp_"} {
		t.Run(prefix, func(t *testing.T) {
			first := state.Party{HostSession: prefix + "first", Name: workerRole}
			second := state.Party{HostSession: prefix + "second", Name: workerRole}
			original := state.Agent{Party: first, Task: "unfinished"}
			st := &state.State{Agents: []state.Agent{original}}
			called := false
			_, err := registerAgent(st, second, func(p state.Party) bool {
				called = true
				if p != first {
					t.Fatalf("liveness checked for %+v", p)
				}
				return true
			}, time.Time{})
			if Code(err) != ExitRefused || !called {
				t.Fatalf("registration: err = %v, liveness called = %v", err, called)
			}
			if !reflect.DeepEqual(st.Agents, []state.Agent{original}) {
				t.Fatalf("refusal changed roster: %+v", st.Agents)
			}

			st.Agents[0].Party = second
			a := &app{}
			if got := a.callerAgent(st, first); got != -1 {
				t.Fatalf("predecessor found successor at %d", got)
			}
			renamed := second
			renamed.Name = renamedApp
			if got := a.callerAgent(st, renamed); got != 0 {
				t.Fatalf("renamed session lookup = %d", got)
			}
			a.as = workerRole
			if got := a.callerAgent(st, first); got != 0 {
				t.Fatalf("explicit --as lookup = %d", got)
			}
			renamedFirst := first
			renamedFirst.Name = renamedApp
			st.Agents[0] = original
			reg, err := registerAgent(st, renamedFirst, func(state.Party) bool {
				t.Fatal("own session should not require liveness")
				return true
			}, time.Time{})
			if err != nil || !reg.own || reg.task != "unfinished" || st.Agents[0].Name != renamedApp {
				t.Fatalf("renamed registration = %+v, %v; roster = %+v", reg, err, st.Agents)
			}
		})
	}
}
