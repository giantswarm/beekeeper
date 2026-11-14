package cmd

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/state"
)

// tellDefaulted runs in the command, which holds what it needs (watchTellDefaulted).
func (w *watcher) tellDefaulted(ctx context.Context, notes []state.Note) {
	watchTellDefaulted(w, ctx, notes)
}
