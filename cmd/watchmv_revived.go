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

// ArchiveWarmGrace is how long the watch leaves running a superseded
// session's CLI the doctor warmed to archive itself: the show, the CLI's
// start and the steward's turn (archiveWait) fit in it.
const ArchiveWarmGrace = 5 * time.Minute

// archiveWarmed says whether s's CLI was warmed within archiveWarmGrace of
// now to archive its own desktop session, which an archive owed records.
func archiveWarmed(st *state.State, s *claude.Session, now time.Time) bool {
	return slices.ContainsFunc(st.Archives, func(ar state.Archive) bool {
		return (ar.Session == s.ID || s.HostID != "" && ar.Host == s.HostID) &&
			!ar.Warmed.IsZero() && now.Sub(ar.Warmed) < ArchiveWarmGrace
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
		by, since := Cmd.Superseded(st, s.Party(), now)
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

// DesktopArchived says whether the desktop records the session host
// archived; a variable for tests.
var DesktopArchived = func(cfg *config.Config, host string) bool {
	r, ok := claude.ReadRecord(cfg, host)
	return ok && r.IsArchived
}

// StopRevived stops each running CLI of a superseded session and its
// desktop's reopen and turn units, owes the archive of its desktop session
// and says REVIVED once per CLI. A CLI the doctor warmed to archive itself
// runs on for archiveWarmGrace; once its desktop session is archived, it is
// stopped as done, logged as agents.archive, never REVIVED. It returns the
// sessions that answer to their name: the running ones but the stopped and
// the warmed.
func (w *Watcher) StopRevived(ctx context.Context, sessions []*claude.Session) []*claude.Session {
	st, err := w.Host.Store().Peek()
	if err != nil {
		return sessions
	}
	revived, warmed := revivedRuns(st, sessions, w.Host.Now())
	drop := map[*claude.Session]bool{}
	for _, s := range warmed {
		drop[s] = true
	}
	for _, r := range revived {
		s := r.session
		key := fmt.Sprintf("revived %s %d", s.ID, s.PID)
		if w.Reported[key] {
			continue
		}
		w.Reported[key], w.Dirty = true, true
		n, err := Cmd.EndRevived(ctx, s)
		if err != nil {
			w.EmitNow("revived", "REVIVED %q: session %s's CLI %d runs although %s carries its name now; stopping it failed: %v",
				s.Name, s.ID, s.PID, r.by, err)
			continue
		}
		drop[s] = true
		lines := Cmd.StopUnits(ctx, s.ID)
		w.Host.RecordStoppedGates(ctx)
		if s.HostID != "" && DesktopArchived(w.Host.Cfg(), s.HostID) {
			_ = w.Host.Store().Record(Cmd.Event(WatchParty, "agents.archive", "%s: session %s's CLI %d stopped: its desktop session %s is archived and %s carries its name",
				s.Name, s.ID, s.PID, s.HostID, r.by))
			continue
		}
		_ = w.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
			Cmd.OweArchive(st, s.Party(), "revived after "+r.by+" took its name", w.Host.Now())
			return []state.Event{Cmd.Event(WatchParty, "agents.revived", "%s: session %s, CLI %d, stopped: %s carries its name", s.Name, s.ID, s.PID, r.by)}, nil
		})
		units := ""
		if len(lines) > 0 {
			units = "; " + strings.Join(lines, "; ")
		}
		w.EmitNow("revived", "REVIVED %q: session %s's CLI %d came back although %s carries its name now: stopped it and %d processes it left%s; the doctor archives its desktop session",
			s.Name, s.ID, s.PID, r.by, n-1, units)
	}
	return slices.DeleteFunc(slices.Clone(sessions), func(s *claude.Session) bool { return drop[s] })
}
