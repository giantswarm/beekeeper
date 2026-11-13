//go:build scenario && linux

package scenario

import (
	"errors"
	"fmt"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// startRewrites starts given.state.rewrite's writer: from now on it rewrites
// the desk's state at that interval, through the state's own lock as every
// writer of a desk does, until stopRewrites.
func (d *driver) startRewrites() {
	g := d.s.Given.State
	if g == nil || g.Rewrite.Duration == 0 {
		return
	}
	stop, done := make(chan struct{}), make(chan struct{})
	d.rewriteStop, d.rewriteDone = stop, done
	go func() {
		defer close(done)
		store, err := state.OpenVersion(stateDir, "")
		if err != nil {
			d.rewriteErr = err
			return
		}
		tick := time.NewTicker(g.Rewrite.Duration)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			now := d.clock.Now().UTC()
			if err := store.Update(func(st *state.State) ([]state.Event, error) {
				if len(st.Agents) == 0 {
					return nil, errors.New("no worker on the roster")
				}
				a := &st.Agents[i%len(st.Agents)]
				a.ActivityAt = now
				return []state.Event{{At: now, By: a.Party, Verb: VerbRewrite, Detail: fmt.Sprintf("rewrite %d", i+1)}}, nil
			}); err != nil {
				d.rewriteErr = fmt.Errorf("rewrite %d: %w", i+1, err)
				return
			}
			d.rewrites.Add(1)
		}
	}()
}

// stopRewrites stops the writer startRewrites started and says why it
// stopped early, if it did.
func (d *driver) stopRewrites() []string {
	if d.rewriteStop == nil {
		return nil
	}
	close(d.rewriteStop)
	<-d.rewriteDone
	d.rewriteStop = nil
	if d.rewriteErr != nil {
		return []string{fmt.Sprintf("given.state.rewrite: %v", d.rewriteErr)}
	}
	return nil
}
