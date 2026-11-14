package cmd

import (
	"time"
)

// docker runs in the command, which holds what it needs (watchDocker).
func (w *watcher) docker(now time.Time) {
	watchDocker(w, now)
}
