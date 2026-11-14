package cmd

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// keepSwap keeps the machine sample's swap reading for the poll and, in the
// state's side file, for capacity.
func (w *watcher) keepSwap(r *capacity.SwapReading) {
	w.mu.Lock()
	w.swap = r
	w.mu.Unlock()
	_ = w.a.store.WriteFile(swapFile, r)
}

// capacity says CAPACITY FULL once at a positive ceiling, with the headroom,
// and ends with one ENDED line when the count recovers; a ceiling of 0 clears
// it and stays silent. The machine is read only at that edge.
func (w *watcher) capacity(ctx context.Context, st *state.State, sessions []*claude.Session) {
	live := w.a.liveCapacity()
	k, u := w.currentUsage(live, w.a.now)
	c := countAgents(st, sessions, w.a.now, w.a.cfg)
	busy := len(c.Busy)
	// A configured capacity.ceiling of 0 keeps the line silent even when
	// the derived ceiling is positive (a self-hosted provider's own ceiling
	// rides in the derived total).
	w.clear("capacity-low")
	if (u != nil && u.Error != "") || k.Ceiling == 0 || live.Ceiling == 0 || busy < k.Ceiling || st.Stop != nil || startsHold(st, w.a.now) != nil {
		w.clear("capacity-full")
		return
	}
	w.mu.Lock()
	swap := w.swap
	w.mu.Unlock()
	var h *capacity.Headroom
	if w.readHeadroom != nil {
		h = w.readHeadroom(ctx, swap)
	} else {
		h = w.a.readHeadroom(ctx, k, swap)
	}
	// The watch weighs a start that does not build: a busy slot is a note.
	v := capacity.NewCapacity(c, k, h, false).WithUsage(u, w.a.now)
	w.check("capacity-full", busy >= k.Ceiling, "CAPACITY FULL %d of %d: %s", busy, k.Ceiling, v.Headroom.Line())
}
