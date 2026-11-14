package cmd

import (
	"github.com/giantswarm/beekeeper/internal/state"
)

// wakeTargetPark finds the current wait using the same name and id
// resolution as wake, including an off-roster session's park.
func wakeTargetPark(st *state.State, q string) *state.Park {
	if i, err := findAgent(st, q); err == nil {
		return st.Agents[i].Park
	}
	if i, ok := offRosterPark(st, q); ok {
		return &st.Parks[i]
	}
	return nil
}
