package cmd

import (
	"time"
)

// diskPrune runs in the command, which holds what it needs (watchDiskPrune).
func (w *watcher) diskPrune(now time.Time, pressure bool) {
	watchDiskPrune(w, now, pressure)
}
