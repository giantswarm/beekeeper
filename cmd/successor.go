package cmd

import (
	"cmp"
	"context"
	"fmt"
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

// takesRelay reports whether me is the session a relay to to names: by its
// ids, or by its run name while to has none, a background successor's
// relay opened before the claude daemon chose its session id.
func takesRelay(to, me state.Party) bool {
	return to.Is(me) || to.Session == "" && to.HostSession == "" && to.Name != "" && strings.EqualFold(to.Name, me.Name)
}

// startSuccessor starts rl's next run as a fresh session and opens the
// relay from from to it, so its `<role> start` takes the role; by is who
// asks: the holder's relay, or the standby watch once from is gone. The
// successor runs in dir (empty: successorDir) with from's model. A start
// that fails withdraws the relay. It returns the successor and the relay's
// line.
func (a *app) startSuccessor(ctx context.Context, rl role, from, by state.Party, dir string) (state.Party, string, error) {
	return a.startRun(ctx, rl, from, by, dir, nil)
}

// startRun is startSuccessor, where from may be the zero party: no holder is
// recorded. An initial run reserves a pending relay too, so another start
// cannot launch the same run while its session is being created. Its
// `<role> start` takes the role as a start of beekeeper's (role.mayTake).
// check, when set, refuses the run under the state's lock before anything starts.
func (a *app) startRun(ctx context.Context, rl role, from, by state.Party, dir string, check func(*state.State) error) (state.Party, string, error) {
	id := uuid.NewString()
	holder := from != (state.Party{})
	// A background successor's id is the claude daemon's: its relay names
	// its run until the start records the id.
	background := a.cfg.Agents.Mode == config.ModeBackground
	var to state.Party
	var msg, startDir, startModel string
	fromLabel := from.Name
	// A machine without a process table shows no session carrying the title.
	sessions, _, _ := a.sessions()
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		if check != nil {
			if err := check(st); err != nil {
				return nil, err
			}
		}
		if s, ok := st.BypassStartOf(from); holder && ok {
			startDir, startModel = s.Dir, s.Model
		}
		r := rl.get(st)
		n := max(rl.lastRun(r), a.titledRun(rl, r.Holder)) + 1
		to = state.Party{Session: id, HostSession: "local_" + id, Name: rl.runName(n)}
		if background {
			to = state.Party{Name: rl.runName(n)}
		}
		if err := titleTaken(sessions, to.Name, to); err != nil {
			return nil, err
		}
		if r.Run > 0 && r.Holder != nil && rl.runName(r.Run) != from.Name {
			fromLabel = fmt.Sprintf("%s (%s)", from.Name, rl.runName(r.Run))
		}
		if !holder {
			r.Relay = &state.Relay{To: to, At: a.now.UTC(), Expires: a.now.Add(rl.cfg(a.cfg).RelayTTL.Duration).UTC()}
			r.Run = n
			rl.set(st, r)
			msg = fmt.Sprintf("no %s is recorded: %q's `beekeeper %s start` takes the role", rl.name, to.Name, rl.name)
			return []state.Event{event(by, rl.name+".run", "starting %s: no %s is recorded", to.Name, rl.name)}, nil
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
	var rec *claude.Record
	if holder {
		rec, _ = claude.ReadRecord(a.cfg, desktopID(from))
	}
	model := strings.TrimSpace(rl.cfg(a.cfg).Model)
	if rec != nil {
		model = cmp.Or(model, rec.Model)
	}
	if dir == "" && rl.cfg(a.cfg).Dir == "" {
		dir = a.cfg.Agents.Dir
	}
	if dir == "" {
		dir = successorDir(rl.cfg(a.cfg), rec, startDir)
	}
	if background {
		sa, err := a.startAgent(ctx, agentStart{by: &by, name: to.Name, role: rl.name, brief: rl.backgroundSuccessorBrief(rl.cfg(a.cfg), to.Name, fromLabel),
			task: fmt.Sprintf("%s as %s", rl.duty, to.Name), dir: dir, model: cmp.Or(model, startModel), mode: config.ModeBackground})
		if err != nil {
			a.withdrawRelay(rl, to, by)
			return state.Party{}, "", fmt.Errorf("starting %q: %w (its relay is withdrawn)", to.Name, err)
		}
		named := state.Party{Session: sa.id, Name: to.Name}
		_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
			if r := rl.get(st); r.Relay != nil && r.Relay.To.Is(to) {
				r.Relay.To = named
				rl.set(st, r)
			}
			return nil, nil
		})
		return named, msg, nil
	}
	// The successor's import goes ahead past the desktop window's focus, as
	// a desktop turn's does: its role turn soon runs headless and may not
	// end before the next relay, and the person sees the role's holder only
	// in the desktop's sidebar.
	_, err = a.startAgent(ctx, agentStart{id: id, by: &by, name: to.Name, role: rl.name, brief: rl.successorBrief(rl.cfg(a.cfg), to.Name, fromLabel),
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
