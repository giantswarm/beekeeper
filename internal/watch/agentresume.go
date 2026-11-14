package watch

import (
	"regexp"

	"github.com/giantswarm/beekeeper/internal/state"
)

// personWaitWords name a person's own word in a park's waits: their answer,
// a review, a reply, a decision.
var personWaitWords = regexp.MustCompile(`(?i)\b(?:notes?|persons?|answers?|reviews?|approvals?|decisions?|repl(?:y|ies))\b`)

// resumedForTask reports whether ag was resumed on a wait for its current
// task already.
func resumedForTask(ag state.Agent) bool {
	return !ag.ResumedWait.IsZero() && !ag.ResumedWait.Before(ag.AssignedAt)
}
