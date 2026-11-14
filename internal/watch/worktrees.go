package watch

import (
	"time"
)

// worktrees runs in the command, which holds what it needs (watchWorktrees).
func (w *Watcher) worktrees(now time.Time) {
	Cmd.WatchWorktrees(w, now)
}
