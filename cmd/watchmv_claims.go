package cmd

import (
	"context"
)

// claims is a watch's sync of the desk's claims: a failure is a condition
// of its own, said once until a sync succeeds.
func (w *Watcher) claims(ctx context.Context) {
	if err := w.Host.syncClaims(ctx); err != nil {
		w.Emit("claims", "CLAIMS on GitHub failed: %v", err)
		return
	}
	w.Clear("claims")
}
