package cmd

import (
	"github.com/giantswarm/beekeeper/internal/claude"
)

// sameTitles runs in the command, which holds what it needs (watchSameTitles).
func (w *watcher) sameTitles(sessions []*claude.Session) {
	watchSameTitles(w, sessions)
}
