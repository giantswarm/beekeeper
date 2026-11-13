package gate

import (
	"fmt"
	"time"

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/merge"
)

// ReadBackoff are the pauses between the tries of a gate read that failed
// in transit: four tries over about two minutes, so a short network or
// Teleport outage does not refuse a merge, nor cost it its lane place.
var ReadBackoff = []time.Duration{10 * time.Second, 30 * time.Second, 80 * time.Second}

// ReadRetrying runs read, the gate's read of what (the GitHub budget, an
// installation's HelmReleases, a pull request), again after each
// ReadBackoff pause while it fails in transit (merge.Transient), a line per
// retry. Any other failure returns at once. The error after the last try
// names the last failure and the tries.
func (g *GateRun) ReadRetrying(what string, read func() error) error {
	for try := 0; ; try++ {
		err := read()
		if err == nil || !merge.Transient(err) || g.Ctx.Err() != nil {
			return err
		}
		if try == len(ReadBackoff) {
			return fmt.Errorf("%w; %d tries failed in transit", err, try+1)
		}
		pause := ReadBackoff[try]
		GateLine("reading %s failed in transit (%v): retrying in %s, retry %d of %d", what, err, pause, try+1, len(ReadBackoff))
		select {
		case <-g.Ctx.Done():
			return err
		case <-clk.After(pause):
		}
	}
}
