package watch

import (
	"context"
)

// doctor runs in the command, which holds what it needs (watchDoctor).
func (w *Watcher) doctor(ctx context.Context) {
	Cmd.WatchDoctor(w, ctx)
}
