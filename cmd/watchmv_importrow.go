package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/giantswarm/beekeeper/internal/state"
)

// importRows starts the import of every rowless agent that has none in
// flight, each on its own: one held by the desktop never holds another
// (standby watch).
func (w *Watcher) importRows(ctx context.Context, st *state.State) {
	if w.Stand.ImportRow == nil || w.Stand.Turning == nil || st.Stop != nil {
		return
	}
	for _, ag := range w.Host.rowless(ctx, st, w.Stand.Turning) {
		if _, busy := w.Stand.importing.LoadOrStore(ag.Session, true); busy {
			continue
		}
		w.Stand.Inflight.Add(1)
		go func() {
			defer w.Stand.Inflight.Done()
			defer w.Stand.importing.Delete(ag.Session)
			line, err := w.Stand.ImportRow(ctx, ag.Session, ag.Name)
			if err != nil {
				w.EmitNow("import-"+ag.Session, "IMPORT MISSED: %q runs headless with no row in the desktop: %v; the next poll tries again", ag.Name, err)
				return
			}
			w.EmitNow("import-"+ag.Session, "IMPORTED: %s", line)
		}()
	}
}

// RowlessAgents says once per worker that its session has no row in the
// desktop (the desktop never imported it: at its cap of CLIs, say) and
// what shows it: the standby watch's import beside its running headless
// turn, the doctor's reopen once the desktop has room. Said while the
// desktop runs; before, the roster and the event log were its only signs.
func (w *Watcher) RowlessAgents(st *state.State) {
	t := w.Table
	if t == nil || !w.Host.desktopMode() || plat.Opener.Running(t).IsZero() {
		return
	}
	said := map[string]bool{}
	var now []string
	for _, ag := range st.Agents {
		if _, ok := rowlessCandidate(st, ag, w.Host.now); !ok || w.Host.hasRow(ag.Session) {
			continue
		}
		said[ag.Session] = true
		if w.RowlessSaid[ag.Session] {
			continue
		}
		how := "no CLI of it runs; the doctor reopens it once the desktop has room"
		if turn := headlessTurn(t, ag.Session); turn != "" {
			how = "its " + turn + " runs headless; the standby watch imports it beside the turn"
		}
		now = append(now, fmt.Sprintf("%q (%s)", ag.Name, how))
	}
	w.RowlessSaid = said
	if len(now) > 0 {
		w.EmitNow("agents-rowless", "NO DESKTOP ROW: the desktop, running %d of its cap of %d CLIs, never imported %s", desktopCLIs(t), w.Host.desktopCap(), strings.Join(now, ", "))
	}
}
