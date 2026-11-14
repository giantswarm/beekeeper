package cmd

import (
	"context"
)

// doctor runs in the command, which holds what it needs (watchDoctor).
func (w *watcher) doctor(ctx context.Context) {
	watchDoctor(w, ctx)
}
