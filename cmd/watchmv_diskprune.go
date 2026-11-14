package cmd

import (
	"time"
)

// DiskPrune runs in the command, which holds what it needs (watchDiskPrune).
func (w *Watcher) DiskPrune(now time.Time, pressure bool) {
	Cmd.WatchDiskPrune(w, now, pressure)
}
