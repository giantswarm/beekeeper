package cmd

import (
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// ungated runs in the command, which holds what it needs (watchUngated).
func (w *watcher) ungated(sessions []*claude.Session, t *proc.Table) {
	watchUngated(w, sessions, t)
}
