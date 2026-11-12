package cmd

import (
	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/state"
)

// reviewWriteRefusal refuses a reviewer on an SSH execution host the
// publication of its fixes, which the coordinator cannot attribute to the
// review yet (https://github.com/giantswarm/beekeeper/issues/3849). A local
// reviewer fixes its findings on the branch and approves the head it pushed
// (https://github.com/giantswarm/beekeeper/issues/3861); the worker hook no
// longer refuses its commits and pushes.
const reviewWriteRefusal = "Refused: review sessions cannot commit or push fixes. Post findings with gh pr review --comment and report the verdict and review URL with beekeeper agents idle --done --report; the supervisor forwards findings to the implementing worker."

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
