package cmd

import (
	"context"
)

// NewGuideRecovery runs in the command, which holds what it needs (watchNewGuideRecovery).
func (w *Watcher) NewGuideRecovery() *Watcher {
	return watchNewGuideRecovery(w)
}

// RecoverGuide runs in the command, which holds what it needs (watchRecoverGuide).
func (w *Watcher) RecoverGuide(ctx context.Context) {
	watchRecoverGuide(w, ctx)
}
