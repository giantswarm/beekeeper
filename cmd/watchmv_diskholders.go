package cmd

import (
	"time"
)

// StateHolders runs in the command, which holds what it needs (watchStateHolders).
func (w *Watcher) StateHolders(now time.Time) {
	watchStateHolders(w, now)
}
