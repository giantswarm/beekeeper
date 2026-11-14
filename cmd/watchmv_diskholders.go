package cmd

import (
	"time"
)

// StateHolders runs in the command, which holds what it needs (watchStateHolders).
func (w *Watcher) StateHolders(now time.Time) {
	Cmd.WatchStateHolders(w, now)
}
