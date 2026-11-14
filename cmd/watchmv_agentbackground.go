package cmd

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// StopParked runs in the command, which holds what it needs (watchStopParked).
func (w *Watcher) StopParked(ctx context.Context, st *state.State, sessions []*claude.Session) {
	Cmd.WatchStopParked(w, ctx, st, sessions)
}
