package cmd

import (
	"fmt"
	"time"

	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
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

// waitBehindLive is a read-only fast path for a merge that already owns its
// place and waits behind a live gate. That predecessor handles recovery and
// GitHub reads: followers need neither those reads nor state rewrites. If
// the predecessor died, normal steps recover its outcome before pruning;
// holds, dropped places, paired refusals and the turn itself are still
// decided by the ordinary gate. claimed says this PID already owns the
// place; a queued handoff still needs the ordinary step's registration.
func (g *gateRun) waitBehindLive() (why string, claimed bool) {
	if !g.placed {
		return "", false
	}
	st, err := g.a.store.Read()
	if err != nil {
		return "", false // the ordinary step reports or re-executes a stale writer
	}
	i := g.mine(st, state.Waiting)
	if i < 0 || g.pairRefusal(st) != "" {
		return "", false
	}
	claimed = st.Merges[i].PID == g.pid
	if !claimed && (g.from == 0 || st.Merges[i].PID != g.from) {
		return "", false
	}
	q := merge.Queue(st, g.lane.Name)
	ahead, behind := q.Ahead(g.repo, g.pr, merge.Unheld(st, g.a.now, g.lane, g.a.present))
	if q.Running != nil {
		ahead, behind = *q.Running, true
	}
	if !behind || !proc.Alive(ahead.PID) {
		return "", false
	}
	return fmt.Sprintf("position %d in lane %s behind %s (%q, %s)", q.Position(g.repo, g.pr), g.lane.Name, ahead.Key(), ahead.By.Name, ahead.Phase), claimed
}
