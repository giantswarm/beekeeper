package cmd

import (
	"fmt"

	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// waitBehindLive is a read-only fast path for a merge that already owns its
// place and waits behind a live gate. That predecessor handles recovery and
// GitHub reads: followers need neither those reads nor state rewrites. If
// the predecessor died, normal steps recover its outcome before pruning;
// holds, dropped places, paired refusals and the turn itself are still
// decided by the ordinary gate. claimed says this PID already owns the
// place; a queued handoff still needs the ordinary step's registration.
func (g *GateRun) waitBehindLive() (why string, claimed bool) {
	if !g.Placed {
		return "", false
	}
	st, err := g.Host.Store().Read()
	if err != nil {
		return "", false // the ordinary step reports or re-executes a stale writer
	}
	i := g.Mine(st, state.Waiting)
	if i < 0 || g.pairRefusal(st) != "" {
		return "", false
	}
	claimed = st.Merges[i].PID == g.Pid
	if !claimed && (g.From == 0 || st.Merges[i].PID != g.From) {
		return "", false
	}
	q := merge.Queue(st, g.Lane.Name)
	ahead, behind := q.Ahead(g.Repo, g.Pr, merge.Unheld(st, g.Host.Now(), g.Lane, g.Host.Present))
	if q.Running != nil {
		ahead, behind = *q.Running, true
	}
	if !behind || !proc.Alive(ahead.PID) {
		return "", false
	}
	return fmt.Sprintf("position %d in lane %s behind %s (%q, %s)", q.Position(g.Repo, g.Pr), g.Lane.Name, ahead.Key(), ahead.By.Name, ahead.Phase), claimed
}
