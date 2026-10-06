package cmd

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A role's runs are numbered: every holder is "<Role> run <n>", the name
// its desktop title, its roster entry and its peer messages share. A relay,
// and the standby watch after a crash, start the next run as a fresh
// session; no session is kept in reserve or repurposed.

// runPattern matches a role run's name.
var runPattern = regexp.MustCompile(`^(\S+) run (\d+)$`)

// runName is the name of rl's run n.
func (rl role) runName(n int) string { return fmt.Sprintf("%s run %d", rl.title, n) }

// runOf is the run of rl that name names, 0 when it names none.
func (rl role) runOf(name string) int {
	m := runPattern.FindStringSubmatch(strings.TrimSpace(name))
	if m == nil || m[1] != rl.title {
		return 0
	}
	n, _ := strconv.Atoi(m[2])
	return n
}

// lastRun is the highest run of rl that r records: its Run, and the names
// of its holder, its relay and the holders it relieved.
func (rl role) lastRun(r state.Role) int {
	n := r.Run
	see := func(p state.Party) { n = max(n, rl.runOf(p.Name)) }
	if r.Holder != nil {
		see(r.Holder.Party)
	}
	if r.Relay != nil {
		see(r.Relay.From)
		see(r.Relay.To)
	}
	for _, rf := range r.Relieved {
		see(rf.Party)
		see(rf.By)
	}
	return n
}

// successorBrief is the first prompt of name, rl's next run after from.
// Its first turn runs headless and only takes the role: a headless turn
// that arms a watch never ends, so the desktop never gets the session's
// CLI and a click on its row would start a second one. The standby watch
// resumes it in its desktop CLI.
func (rl role) successorBrief(name, from string) string {
	return fmt.Sprintf(`You are %q, the next %s run, a fresh session beekeeper started to take over %s from %q.

This first turn runs headless from the command line. In it, only take the role: run `+"`beekeeper %s start`"+`, then end the turn. Arm no Monitor, watch or background task in it: the desktop gets this session's CLI only once this turn has ended.

Once the desktop runs your CLI, a message tells you so there: then run `+"`%s`"+` and follow it. Peers message you as %q.`,
		name, rl.name, rl.duty, from, rl.name, rl.handover, name)
}

// startSuccessor starts rl's next run as a fresh session and opens the
// relay from from to it, so its `<role> start` takes the role; by is who
// asks: the holder's relay, or the standby watch once from is gone. The
// successor runs in dir (empty: successorDir) with from's model. A start
// that fails withdraws the relay. It returns the successor and the relay's
// line.
func (a *app) startSuccessor(ctx context.Context, rl role, from, by state.Party, dir string) (state.Party, string, error) {
	id := uuid.NewString()
	var to state.Party
	var msg, startDir string
	fromLabel := from.Name
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		if s, ok := st.BypassStart(from.Session); ok {
			startDir = s.Dir
		}
		r := rl.get(st)
		n := max(rl.lastRun(r), a.titledRun(rl, r.Holder)) + 1
		to = state.Party{Session: id, HostSession: "local_" + id, Name: rl.runName(n)}
		if r.Run > 0 && r.Holder != nil && rl.runName(r.Run) != from.Name {
			fromLabel = fmt.Sprintf("%s (%s)", from.Name, rl.runName(r.Run))
		}
		var evs []state.Event
		var err error
		msg, evs, err = rl.relay(st, from, to, a.now, rl.cfg(a.cfg).RelayTTL.Duration)
		for i := range evs {
			evs[i].By = by
		}
		return evs, err
	})
	if err != nil {
		return state.Party{}, "", err
	}
	rec, _ := claude.ReadRecord(a.cfg, desktopID(from))
	var model string
	if rec != nil {
		model = rec.Model
	}
	if dir == "" && rl.cfg(a.cfg).Dir == "" {
		dir = a.cfg.Agents.Dir
	}
	if dir == "" {
		dir = successorDir(rl.cfg(a.cfg), rec, startDir)
	}
	// The successor's import goes ahead past the desktop window's focus, as
	// a desktop turn's does: its role turn soon runs headless and may not
	// end before the next relay, and the person sees the role's holder only
	// in the desktop's sidebar.
	_, err = a.startAgent(ctx, agentStart{id: id, by: &by, name: to.Name, brief: rl.successorBrief(to.Name, fromLabel),
		task: fmt.Sprintf("%s as %s", rl.duty, to.Name), dir: dir, model: model, desktop: true, headless: true})
	if err != nil {
		a.withdrawRelay(rl, to, by)
		return state.Party{}, "", fmt.Errorf("starting %q: %w (its relay is withdrawn)", to.Name, err)
	}
	return to, msg, nil
}

// successorDir is the folder a successor of the holder whose desktop
// record is rec (nil: none) starts in: the role's configured dir, else the
// folder the holder's desktop session started from, else the folder
// beekeeper started the holder in (start), else the caller's. Never the
// worktree the desktop made for the holder: the desktop warms a session in
// such a folder on a worktree of the folder's branch, which the holder's
// worktree still has checked out, so the successor's desktop CLI never
// starts.
func successorDir(cfg config.Role, rec *claude.Record, start string) string {
	switch {
	case cfg.Dir != "":
		return cfg.Dir
	case rec != nil && rec.OriginCwd != "":
		return rec.OriginCwd
	case rec != nil && rec.Cwd != "":
		return rec.Cwd
	case start != "":
		return start
	}
	return "."
}

// withdrawRelay drops rl's relay to to while it is not taken.
func (a *app) withdrawRelay(rl role, to, by state.Party) {
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		r := rl.get(st)
		if r.Relay == nil || !r.Relay.To.Is(to) || !r.Relay.Taken.IsZero() {
			return nil, nil
		}
		r.Relay = nil
		rl.set(st, r)
		return []state.Event{event(by, rl.name+".relay-cancel", "to %s: its session did not start", to.Name)}, nil
	})
}

// titledRun is the run of rl the desktop title of h names: a holder that
// started before the runs were numbered may carry one there only.
func (a *app) titledRun(rl role, h *state.Supervisor) int {
	if h == nil {
		return 0
	}
	if r, ok := claude.ReadRecord(a.cfg, desktopID(h.Party)); ok {
		return rl.runOf(r.Title)
	}
	return 0
}

// desktopID is p's desktop session id: its own, else the one the desktop
// gives a session it imported by its CLI id.
func desktopID(p state.Party) string {
	if p.HostSession != "" {
		return p.HostSession
	}
	return "local_" + p.Session
}
