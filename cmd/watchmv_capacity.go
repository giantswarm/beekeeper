package cmd

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// KeepSwap keeps the machine sample's swap reading for the poll and, in the
// state's side file, for capacity.
func (w *Watcher) KeepSwap(r *capacity.SwapReading) {
	w.Mu.Lock()
	w.swap = r
	w.Mu.Unlock()
	_ = w.Host.store.WriteFile(swapFile, r)
}

// Capacity says CAPACITY FULL once at a positive ceiling, with the headroom,
// and ends with one ENDED line when the count recovers; a ceiling of 0 clears
// it and stays silent. The machine is read only at that edge.
func (w *Watcher) Capacity(ctx context.Context, st *state.State, sessions []*claude.Session) {
	live := w.Host.liveCapacity()
	k, u := w.CurrentUsage(live, w.Host.now)
	c := countAgents(st, sessions, w.Host.now, w.Host.cfg)
	busy := len(c.Busy)
	// A configured capacity.ceiling of 0 keeps the line silent even when
	// the derived ceiling is positive (a self-hosted provider's own ceiling
	// rides in the derived total).
	w.Clear("capacity-low")
	if (u != nil && u.Error != "") || k.Ceiling == 0 || live.Ceiling == 0 || busy < k.Ceiling || st.Stop != nil || startsHold(st, w.Host.now) != nil {
		w.Clear("capacity-full")
		return
	}
	w.Mu.Lock()
	swap := w.swap
	w.Mu.Unlock()
	var h *capacity.Headroom
	if w.ReadHeadroom != nil {
		h = w.ReadHeadroom(ctx, swap)
	} else {
		h = w.Host.readHeadroom(ctx, k, swap)
	}
	// The watch weighs a start that does not build: a busy slot is a note.
	v := capacity.NewCapacity(c, k, h, false).WithUsage(u, w.Host.now)
	w.Check("capacity-full", busy >= k.Ceiling, "CAPACITY FULL %d of %d: %s", busy, k.Ceiling, v.Headroom.Line())
}
