package cmd

import (
	"strings"

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

// timerSettlesPark permits a timer for a plain external wait or a typed
// condition on the very reference the park names. Named waits on a note,
// hold, lease or alert belong to their own settlement machinery. A plain
// wait on a person belongs to that person, never a reminder timer.
func timerSettlesPark(t state.Timer, p *state.Park, person string) bool {
	if parkOnPerson(p, person) {
		return false
	}
	if p.On == "" {
		return true
	}
	kind, ref, _ := strings.Cut(t.When, " ")
	switch kind {
	case "pr-merged", "pr-checks", "issue-closed":
		return strings.TrimSpace(ref) == p.On
	}
	return false
}
