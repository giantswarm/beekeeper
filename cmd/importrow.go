package cmd

import (
	"context"
	"fmt"
	"sync"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// importRowWait bounds one import of a session beside its headless turn:
// the wait for the person's typing to pause, the link and the desktop
// writing its record.
const importRowWait = desktopTurnWait + 2*focusWait + twinWait

// linkMu serializes the claude:// links of this process's imports: each
// switches the desktop's main window to its session and back, and two at
// once would switch it back to each other's. Only the link is serialized,
// never the waits before it.
var linkMu sync.Mutex

// hasRow reports whether the desktop holds a record (a sidebar row) of
// session id.
func (a *app) hasRow(id string) bool {
	_, ok := claude.ReadRecord(a.cfg, "local_"+id)
	return ok
}

// importRow gives session id, which runs a headless turn of beekeeper's
// start or wake and has no row in the desktop, its row under name now,
// beside that turn: the import waits for the person's typing to pause, at
// most desktopTurnWait, and never for the desktop window's focus, since the
// turn may run for hours and a session the person cannot see is worse than
// a brief switch of the window, which goes back to the session it showed.
// The turn stays the session's only CLI (importBeside).
func (a *app) importRow(ctx context.Context, id, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, importRowWait)
	defer cancel()
	d, err := a.watchDesk(ctx)
	if err != nil {
		return "", err
	}
	d.urgent = func() bool { return true }
	err = d.await(ctx, d.turnWait()+awayPoll, a.importWaits(id, name, d.turnWait()))
	a.importEnded(id)
	if err != nil {
		return "", err
	}
	linkMu.Lock()
	defer linkMu.Unlock()
	if a.hasRow(id) {
		return fmt.Sprintf("%s has its row in the desktop already", name), nil
	}
	var sa startedAgent
	if err := a.importBeside(ctx, d, id, name, "", &sa); err != nil {
		return "", err
	}
	line := fmt.Sprintf("imported %s into the desktop beside its headless turn; %s; %s", name, titleLine(name, sa.title), twinLine(sa.twin))
	if l := a.keepImport(ctx, id, name, &sa); l != "" {
		line += "; " + l
	}
	return line, nil
}

// rowless are the agents of beekeeper's starts on the roster whose session
// runs a headless turn and has no row in the desktop: the person sees none
// of their work.
func (a *app) rowless(ctx context.Context, st *state.State, turning func(context.Context, string) bool) []state.Agent {
	var out []state.Agent
	for _, ag := range st.Agents {
		if ag.Session == "" || !started(st, ag.Party) || a.hasRow(ag.Session) || !turning(ctx, ag.Session) {
			continue
		}
		out = append(out, ag)
	}
	return out
}

// importRows starts the import of every rowless agent that has none in
// flight, each on its own: one held by the desktop never holds another
// (standby watch).
func (w *watcher) importRows(ctx context.Context, st *state.State) {
	if w.stand.importRow == nil || w.stand.turning == nil {
		return
	}
	for _, ag := range w.rowless(ctx, st, w.stand.turning) {
		if _, busy := w.stand.importing.LoadOrStore(ag.Session, true); busy {
			continue
		}
		w.stand.inflight.Add(1)
		go func() {
			defer w.stand.inflight.Done()
			defer w.stand.importing.Delete(ag.Session)
			line, err := w.stand.importRow(ctx, ag.Session, ag.Name)
			if err != nil {
				w.emitNow("import-"+ag.Session, "IMPORT MISSED: %q runs headless with no row in the desktop: %v; the next poll tries again", ag.Name, err)
				return
			}
			w.emitNow("import-"+ag.Session, "IMPORTED: %s", line)
		}()
	}
}

// importRowFromWatch is the standby watch's importRow, which logs the
// outcome.
func (a *app) importRowFromWatch(ctx context.Context, id, name string) (string, error) {
	line, err := a.importRow(ctx, id, name)
	_ = a.store.Update(func(*state.State) ([]state.Event, error) {
		if err != nil {
			return []state.Event{event(watchParty, "agent.import", "%s: missed: %v", name, err)}, nil
		}
		return []state.Event{event(watchParty, "agent.import", "%s", line)}, nil
	})
	return line, err
}
