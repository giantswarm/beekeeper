package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

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
// hand-over's predecessor, never a relay's relieved run, which the person
// closes. warmed are those left running as the doctor warmed them to
// archive themselves.
func revivedRuns(st *state.State, sessions []*claude.Session, now time.Time) (out []revivedRun, warmed []*claude.Session) {
	for _, s := range sessions {
		if s.ID == "" || s.Harness != "" {
			continue
		}
		by, since := superseded(st, s.Party(), now)
		switch {
		case by == "":
			continue
		case !since.IsZero():
			continue // a relieved run: the person decides when it goes
		case archiveWarmed(st, s, now):
			warmed = append(warmed, s)
			continue
		}
		out = append(out, revivedRun{session: s, by: by})
	}
	return out, warmed
}

// desktopArchived says whether the desktop records the session host
// archived; a variable for tests.
var desktopArchived = func(cfg *config.Config, host string) bool {
	r, ok := claude.ReadRecord(cfg, host)
	return ok && r.IsArchived
}

// stopRevived stops each running CLI of a superseded session and its
// desktop's reopen and turn units, owes the archive of its desktop session
// and says REVIVED once per CLI. A CLI the doctor warmed to archive itself
// runs on for archiveWarmGrace; once its desktop session is archived, it is
// stopped as done, logged as agents.archive, never REVIVED. It returns the
// sessions that answer to their name: the running ones but the stopped and
// the warmed.
func (w *watcher) stopRevived(ctx context.Context, sessions []*claude.Session) []*claude.Session {
	st, err := w.a.store.Peek()
	if err != nil {
		return sessions
	}
	revived, warmed := revivedRuns(st, sessions, w.a.now)
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
		w.a.recordStoppedGates(ctx)
		if s.HostID != "" && desktopArchived(w.a.cfg, s.HostID) {
			_ = w.a.store.Record(event(watchParty, "agents.archive", "%s: session %s's CLI %d stopped: its desktop session %s is archived and %s carries its name",
				s.Name, s.ID, s.PID, s.HostID, r.by))
			continue
		}
		_ = w.a.store.Update(func(st *state.State) ([]state.Event, error) {
			oweArchive(st, s.Party(), "revived after "+r.by+" took its name", w.a.now)
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
