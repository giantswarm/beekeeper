package cmd

import (
	"context"
)

// claims is a watch's sync of the desk's claims: a failure is a condition
// of its own, said once until a sync succeeds.
func (w *watcher) claims(ctx context.Context) {
	if err := w.a.syncClaims(ctx); err != nil {
		w.emit("claims", "CLAIMS on GitHub failed: %v", err)
		return
	}
	w.clear("claims")
}
