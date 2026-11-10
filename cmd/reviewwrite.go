package cmd

import (
	"os"

	"github.com/giantswarm/beekeeper/internal/state"
)

const reviewWriteRefusal = "Refused: review sessions cannot commit or push fixes. Post findings with gh pr review --comment and report the verdict and review URL with beekeeper agents idle --done --report; the supervisor forwards findings to the implementing worker."

func reviewerSession(st *state.State, by state.Party) bool {
	for _, start := range st.Starts {
		if start.Kind == taskKindReview && start.Party.Is(by) {
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

func (a *app) hookReviewWrite(session string) string {
	if session == "" {
		return ""
	}
	st, err := a.store.Read()
	if err != nil {
		return "Refused: cannot read the review assignment; retry when state is readable."
	}
	if reviewerSession(st, state.Party{Session: session, HostSession: os.Getenv("CLAUDE_CODE_HOST_SESSION_ID")}) {
		return reviewWriteRefusal
	}
	return ""
}
