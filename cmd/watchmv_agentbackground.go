package cmd

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// stopParked runs in the command, which holds what it needs (watchStopParked).
func (w *watcher) stopParked(ctx context.Context, st *state.State, sessions []*claude.Session) {
	watchStopParked(w, ctx, st, sessions)
}
