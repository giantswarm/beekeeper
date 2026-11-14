package cmd

import (
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// Ungated runs in the command, which holds what it needs (watchUngated).
func (w *Watcher) Ungated(sessions []*claude.Session, t *proc.Table) {
	Cmd.WatchUngated(w, sessions, t)
}
