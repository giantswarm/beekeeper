package cmd

import (
	"time"
)

// stateHolders runs in the command, which holds what it needs (watchStateHolders).
func (w *watcher) stateHolders(now time.Time) {
	watchStateHolders(w, now)
}
