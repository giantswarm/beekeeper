package watch

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/state"
)

// TellDefaulted runs in the command, which holds what it needs (watchTellDefaulted).
func (w *Watcher) TellDefaulted(ctx context.Context, notes []state.Note) {
	Cmd.WatchTellDefaulted(w, ctx, notes)
}
