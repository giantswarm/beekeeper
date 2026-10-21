package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A hand-over ends the old session's CLI and a relay ends the relieved
// run's work, but the desktop warms a CLI of a session again whenever it
// shows or reopens it. Such a revived CLI carries the name its successor
// now holds: a message by that name reaches neither for sure, the watch says
// SAME TITLE, and agents resume refuses the ambiguous name. The watch stops
// it, so only the successor answers to the name.

// reliefGrace is how long a relieved run's CLI runs on after its successor
// took the relay before the watch stops it: the run ends the turn it relayed
// in.
const reliefGrace = 2 * time.Minute

// superseded says who carries p's name or role in its place now that its
// session's work ended: the follow-up of a hand-over (a later start, or
// another session on the roster, under its name) or the successor of a
// relay that relieved it. "" when p's session is still its own: it is on the
// roster, holds a role, or beekeeper started no session under its name.
// since is when the relief was taken; zero for a hand-over.
func superseded(st *state.State, p state.Party, now time.Time) (by string, since time.Time) {
	if slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) }) || holdsRole(st, p) {
		return "", time.Time{}
	}
	if pastRun(st, p, now) {
		for _, rl := range roles {
			if r := relievedIn(rl.get(st), p); r != nil && !r.Taken.IsZero() {
				return fmt.Sprintf("%q, who took the %s's role", r.By.Name, rl.name), r.Taken
			}
		}
	}
	i := slices.IndexFunc(st.Starts, func(x state.Start) bool { return x.Session != "" && x.Session == p.Session })
	if i < 0 || !handedOver(st, st.Starts[i]) {
		return "", time.Time{}
	}
	name := st.Starts[i].Name
	if j := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == name }); j >= 0 {
		return fmt.Sprintf("%q, session %s", name, st.Agents[j].Session), time.Time{}
	}
	return fmt.Sprintf("a later start of %q", name), time.Time{}
}

// archiveWarmGrace is how long the watch leaves running a superseded
// session's CLI the doctor warmed to archive itself: the show, the CLI's
// start and the steward's turn (archiveWait) fit in it.
const archiveWarmGrace = 5 * time.Minute

// archiveWarmed says whether s's CLI was warmed within archiveWarmGrace of
// now to archive its own desktop session, which an archive owed records.
func archiveWarmed(st *state.State, s *claude.Session, now time.Time) bool {
	return slices.ContainsFunc(st.Archives, func(ar state.Archive) bool {
		return (ar.Session == s.ID || s.HostID != "" && ar.Host == s.HostID) &&
			!ar.Warmed.IsZero() && now.Sub(ar.Warmed) < archiveWarmGrace
	})
}

// revivedRun is a running CLI of a superseded session: by carries its name
// now.
type revivedRun struct {
	session *claude.Session
	by      string
}

// revivedRuns are the running CLIs of superseded sessions to stop at now: a
// hand-over's predecessor at once, a relieved run once reliefGrace has
// passed since its relief and its CLI runs no tool command. warmed are those
// left running as the doctor warmed them to archive themselves.
func revivedRuns(st *state.State, sessions []*claude.Session, now time.Time) (out []revivedRun, warmed []*claude.Session) {
	for _, s := range sessions {
		if s.ID == "" || s.Harness != "" {
			continue
		}
		by, since := superseded(st, s.Party(), now)
		switch {
		case by == "":
			continue
		case !since.IsZero() && (now.Sub(since) < reliefGrace || len(s.Commands) > 0):
			continue
		case archiveWarmed(st, s, now):
			warmed = append(warmed, s)
			continue
		}
		out = append(out, revivedRun{session: s, by: by})
	}
	return out, warmed
}

// endRevived stops a revived CLI; a variable for tests.
var endRevived = endSession

// stopRevived stops each running CLI of a superseded session and its
// desktop's reopen and turn units, owes the archive of its desktop session
// and says REVIVED once per CLI. A CLI the doctor warmed to archive itself
// runs on for archiveWarmGrace. It returns the sessions that answer to their
// name: the running ones but the stopped and the warmed.
func (w *watcher) stopRevived(ctx context.Context, sessions []*claude.Session) []*claude.Session {
	st, err := w.store.Peek()
	if err != nil {
		return sessions
	}
	revived, warmed := revivedRuns(st, sessions, w.now)
	drop := map[*claude.Session]bool{}
	for _, s := range warmed {
		drop[s] = true
	}
	for _, r := range revived {
		s := r.session
		key := fmt.Sprintf("revived %s %d", s.ID, s.PID)
		if w.reported[key] {
			continue
		}
		w.reported[key], w.dirty = true, true
		n, err := endRevived(ctx, s)
		if err != nil {
			w.emitNow("revived", "REVIVED %q: session %s's CLI %d runs although %s carries its name now; stopping it failed: %v",
				s.Name, s.ID, s.PID, r.by, err)
			continue
		}
		drop[s] = true
		lines := stopUnits(ctx, s.ID)
		_ = w.store.Update(func(st *state.State) ([]state.Event, error) {
			oweArchive(st, s.Party(), "revived after "+r.by+" took its name", w.now)
			return []state.Event{event(watchParty, "agents.revived", "%s: session %s, CLI %d, stopped: %s carries its name", s.Name, s.ID, s.PID, r.by)}, nil
		})
		units := ""
		if len(lines) > 0 {
			units = "; " + strings.Join(lines, "; ")
		}
		w.emitNow("revived", "REVIVED %q: session %s's CLI %d came back although %s carries its name now: stopped it and %d processes it left%s; the doctor archives its desktop session",
			s.Name, s.ID, s.PID, r.by, n-1, units)
	}
	return slices.DeleteFunc(slices.Clone(sessions), func(s *claude.Session) bool { return drop[s] })
}

// stopUnits stops the start and wake units of session id still in their
// turn and its reopen units, which would show it in the desktop and warm a
// CLI of it again: the turns' units first, as their stop-posts start reopen
// units. It returns one line per unit.
func stopUnits(ctx context.Context, id string) []string {
	var out []string
	stop := func(u string) {
		if err := plat.Launcher.Stop(ctx, u); err != nil {
			out = append(out, fmt.Sprintf("stopping %s of session %s: %v", u, id, err))
			return
		}
		out = append(out, fmt.Sprintf("stopped %s of session %s, so the desktop does not reopen it", u, id))
	}
	for _, u := range turningUnits(ctx, id) {
		stop(u)
	}
	for _, u := range reopenUnits(ctx, id) {
		stop(u)
	}
	return out
}
