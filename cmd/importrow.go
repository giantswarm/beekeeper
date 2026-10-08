package cmd

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
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

// rowGrace is how long after its start a session may have no row in the
// desktop before the doctor reopens it: a start's seed turn, its import
// and the delivery of its task turn take that long at most, and the reopen
// after a headless task turn imports the session itself.
const rowGrace = replyWait + importRowWait

// reopenUnitPrefix is the unit prefix of every reopen: the one a start's or
// wake's stop-post starts once the turn ended (agents reopen --detach), and
// the doctor's for a rowless worker.
const reopenUnitPrefix = "beekeeper-reopen-"

// reopenPrefix is the unit prefix of the reopens of session id; reopenUnit
// a new unit name for each, as wakeUnit (an ended unit stays loaded while a
// process it started runs on).
func reopenPrefix(id string) string { return reopenUnitPrefix + id[:min(8, len(id))] }

func reopenUnit(id string) string { return reopenPrefix(id) + "-" + uuid.NewString()[:8] }

// reopenUnits are the reopen units of session id that run or stop.
func reopenUnits(ctx context.Context, id string) []string {
	return plat.Launcher.Running(ctx, true, reopenPrefix(id)+"*")
}

// rowlessWorker is a worker of beekeeper's starts whose session the desktop
// never imported: it has no row in the sidebar, so nobody sees it there,
// reads its transcript or types into it.
type rowlessWorker struct {
	agent state.Agent
	start state.Start
}

// rowlessCandidate is the start of a worker (busy or parked on a task, not
// done, holding no role) of beekeeper's whose session may lack its row,
// past rowGrace since the start; false for any other agent. A finished
// worker is the doctor's archive chore, a role holder the standby watch's.
func rowlessCandidate(st *state.State, ag state.Agent, now time.Time) (state.Start, bool) {
	if ag.Task == "" || ag.Done || ag.Session == "" || holdsRole(st, ag.Party) {
		return state.Start{}, false
	}
	i := slices.IndexFunc(st.Starts, func(s state.Start) bool { return s.Session == ag.Session })
	if i < 0 || now.Sub(st.Starts[i].At) < rowGrace {
		return state.Start{}, false
	}
	return st.Starts[i], true
}

// rowlessWorkers are the candidates (rowlessCandidate) that have no row in
// the desktop and no CLI running, neither a headless turn (the standby
// watch imports them beside it, importRows) nor a desktop CLI, and no
// reopen of theirs running: only a reopen shows them again. record reads a
// desktop session's record.
func (a *app) rowlessWorkers(ctx context.Context, st *state.State, t *proc.Table, record func(host string) (*claude.Record, bool)) []rowlessWorker {
	if t == nil {
		return nil
	}
	var out []rowlessWorker
	for _, ag := range st.Agents {
		start, ok := rowlessCandidate(st, ag, a.now)
		if !ok {
			continue
		}
		if _, ok := record("local_" + ag.Session); ok {
			continue
		}
		if headlessTurn(t, ag.Session) != "" || desktopTwin(t, ag.Session) != nil || unitsReopening(ctx, ag.Session) {
			continue
		}
		out = append(out, rowlessWorker{agent: ag, start: start})
	}
	return out
}

// reopenRowless reopens the rowless workers in the desktop while it runs
// fewer CLIs than its cap, each `agents reopen` in a transient unit of its
// own that outlives the doctor (it waits for the person to leave the
// desktop's window, up to reopenAwayWait, and shows the session, which
// gives it its row and warms its CLI), one agent.reopen event each, and
// returns the doctor's lines. A dry run says what it would reopen, and whom
// the cap, or a desktop that does not run, leaves without a row.
func (a *app) reopenRowless(ctx context.Context, st *state.State, t *proc.Table, record func(host string) (*claude.Record, bool), r doctorRun) []string {
	workers := a.rowlessWorkers(ctx, st, t, record)
	if len(workers) == 0 {
		return nil
	}
	var lines []string
	stays := func(w rowlessWorker, why string) {
		if r.dryRun {
			lines = append(lines, fmt.Sprintf("would leave %q without its desktop row: %s", w.agent.Name, why))
		}
	}
	if plat.Opener.Running(t).IsZero() {
		for _, w := range workers {
			stays(w, "the desktop does not run")
		}
		return lines
	}
	// A reopen under way (a turn's stop-post's, an earlier pass's) warms a
	// CLI the process table does not show yet: it counts against the cap as
	// well.
	running, pending, limit := desktopCLIs(t), len(plat.Launcher.Running(ctx, true, reopenUnitPrefix+"*")), a.desktopCap()
	n := running + pending
	for _, w := range workers {
		if n >= limit {
			stays(w, fmt.Sprintf("the desktop runs its cap of %d CLIs", limit))
			continue
		}
		why := fmt.Sprintf("no row in the desktop since its start at %s and no CLI of it runs; the desktop runs %d of its cap of %d CLIs", clock(a.now, w.start.At), running, limit)
		if pending > 0 {
			why += fmt.Sprintf(", %s under way", plural(pending, "reopen"))
		}
		n++ // the show warms its CLI
		if r.dryRun {
			lines = append(lines, fmt.Sprintf("would reopen %q in the desktop (%s)", w.agent.Name, why))
			continue
		}
		unit := reopenUnit(w.agent.Session)
		if err := a.launchReopen(unit, w.start.Dir, w.agent.Session, ""); err != nil {
			lines = append(lines, fmt.Sprintf("%q: its reopen did not start: %v", w.agent.Name, err))
			n--
			continue
		}
		line := fmt.Sprintf("reopens %q in the desktop, which gives it its row and warms its CLI (%s; unit %s)", w.agent.Name, why, unit)
		lines = append(lines, line)
		_ = a.store.Update(func(*state.State) ([]state.Event, error) {
			return []state.Event{event(r.by, "agent.reopen", "%s: %s", w.agent.Name, line)}, nil
		})
	}
	return lines
}

// launchReopen starts `agents reopen` of arg (a session id, or a wake's
// local_ desktop id) in unit, in dir: a start's or wake's stop-post
// (detachReopen; turn is its unit) or the doctor (reopenRowless; none). The
// unit runs reopenAwayWait + stopPostWait at most (RuntimeMaxSec): the wait
// for the person to leave the desktop's window, then the show, the CLI it
// warms and the retitle; a stop (a hand-over's) ends it at once, as asked,
// its short-lived children (the desktop's opener, hyprctl) with it.
func (a *app) launchReopen(unit, dir, arg, turn string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	argv := []string{self, agentsName, reopenName}
	if turn != "" {
		argv = append(argv, "--"+turnFlag, turn)
	}
	return startUnit(platform.Unit{Name: unit, Dir: dir, Argv: append(argv, arg), TermIsSuccess: true, MaxRuntime: reopenAwayWait + stopPostWait}, a.explicitConfig())
}

// rowlessAgents says once per worker that its session has no row in the
// desktop (the desktop never imported it: at its cap of CLIs, say) and
// what shows it: the standby watch's import beside its running headless
// turn, the doctor's reopen once the desktop has room. Said while the
// desktop runs; before, the roster and the event log were its only signs.
func (w *watcher) rowlessAgents(st *state.State) {
	t := w.table
	if t == nil || plat.Opener.Running(t).IsZero() {
		return
	}
	said := map[string]bool{}
	var now []string
	for _, ag := range st.Agents {
		if _, ok := rowlessCandidate(st, ag, w.now); !ok || w.hasRow(ag.Session) {
			continue
		}
		said[ag.Session] = true
		if w.rowlessSaid[ag.Session] {
			continue
		}
		how := "no CLI of it runs; the doctor reopens it once the desktop has room"
		if turn := headlessTurn(t, ag.Session); turn != "" {
			how = "its " + turn + " runs headless; the standby watch imports it beside the turn"
		}
		now = append(now, fmt.Sprintf("%q (%s)", ag.Name, how))
	}
	w.rowlessSaid = said
	if len(now) > 0 {
		w.emitNow("agents-rowless", "NO DESKTOP ROW: the desktop, running %d of its cap of %d CLIs, never imported %s", desktopCLIs(t), w.desktopCap(), strings.Join(now, ", "))
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
