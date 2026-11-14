package cmd

import (
	"time"
)

// worktrees runs in the command, which holds what it needs (watchWorktrees).
func (w *watcher) worktrees(now time.Time) {
	watchWorktrees(w, now)
}
