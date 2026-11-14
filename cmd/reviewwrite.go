package cmd

import (
	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/state"
)

func reviewerSession(st *state.State, by state.Party) bool {
	for _, start := range st.Starts {
		if start.Kind == capacity.TaskKindReview && start.Is(by) {
			return true
		}
	}
	for _, round := range st.DeskReviews {
		if round.Reviewer.Is(by) {
			return true
		}
	}
	return false
}
