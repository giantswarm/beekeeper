package cmd

import (
	"time"

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// stopWait is how long a TaskStop waits for an ended devctl's
// merge-child to exit before it asks GitHub how the merge stands.
var stopWait = 5 * time.Second

// waitGone says whether pid exits within d.
func waitGone(pid int, d time.Duration) bool {
	for end := clk.Now().Add(d); proc.Alive(pid); clk.Sleep(50 * time.Millisecond) {
		if !clk.Now().Before(end) {
			return false
		}
	}
	return true
}
