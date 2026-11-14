package cmd

import (
	"context"
)

// newGuideRecovery runs in the command, which holds what it needs (watchNewGuideRecovery).
func (w *watcher) newGuideRecovery() *watcher {
	return watchNewGuideRecovery(w)
}

// recoverGuide runs in the command, which holds what it needs (watchRecoverGuide).
func (w *watcher) recoverGuide(ctx context.Context) {
	watchRecoverGuide(w, ctx)
}
