package cmd

import (
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// stoppedAgents are the agents with a task whose CLI does not run.
func stoppedAgents(agents []state.Agent, sessions []*claude.Session) []state.Agent {
	var out []state.Agent
	for _, ag := range agents {
		if ag.Execution != nil {
			continue
		}
		if _, live := claude.Live(sessions, ag.Party); ag.Task != "" && !live {
			out = append(out, ag)
		}
	}
	return out
}
