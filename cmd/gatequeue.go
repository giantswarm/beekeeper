package cmd

import (
	"time"
)

// gateQueuePause backs off a passive queue reader to one minute. The head
// keeps its five-second checks; the caller bounds every sleep by its own
// deadline and context. A live gate keeps its place without Seen writes
// (merge.Present and merge.Prune both check its PID).
func gateQueuePause(previous time.Duration, passive bool) time.Duration {
	if !passive || previous == 0 {
		return 5 * time.Second
	}
	return min(2*previous, time.Minute)
}
