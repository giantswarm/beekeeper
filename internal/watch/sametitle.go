package watch

import (
	"github.com/giantswarm/beekeeper/internal/claude"
)

// SameTitles runs in the command, which holds what it needs (watchSameTitles).
func (w *Watcher) SameTitles(sessions []*claude.Session) {
	Cmd.WatchSameTitles(w, sessions)
}
