package cmd

import (
	"fmt"
	"time"

	"github.com/giantswarm/beekeeper/internal/merge"
)

// readBackoff are the pauses between the tries of a gate read that failed
// in transit: four tries over about two minutes, so a short network or
// Teleport outage does not refuse a merge, nor cost it its lane place.
var readBackoff = []time.Duration{10 * time.Second, 30 * time.Second, 80 * time.Second}

// readRetrying runs read, the gate's read of what (the GitHub budget, an
// installation's HelmReleases, a pull request), again after each
// readBackoff pause while it fails in transit (merge.Transient), a line per
// retry. Any other failure returns at once. The error after the last try
// names the last failure and the tries.
func (g *gateRun) readRetrying(what string, read func() error) error {
	for try := 0; ; try++ {
		err := read()
		if err == nil || !merge.Transient(err) || g.ctx.Err() != nil {
			return err
		}
		if try == len(readBackoff) {
			return fmt.Errorf("%w; %d tries failed in transit", err, try+1)
		}
		pause := readBackoff[try]
		gateLine("reading %s failed in transit (%v): retrying in %s, retry %d of %d", what, err, pause, try+1, len(readBackoff))
		select {
		case <-g.ctx.Done():
			return err
		case <-time.After(pause):
		}
	}
}
