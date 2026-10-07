package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// An archive owed is the desktop session of an agent that left the roster
// unarchived: its CLI ran a turn, or no steward recorded the archive. The
// doctor asks a steward for it again on its later runs while the CLI runs
// no turn, until the desktop records it, within archiveTries stewards'
// turns and archiveOwedFor. A run that asked no steward (the desktop did
// not run, the person typed) counts no turn.
const (
	archiveTries   = 5
	archiveOwedFor = 24 * time.Hour
	// archiveAgain is how long the doctor leaves an archive a steward did
	// not record before it asks again.
	archiveAgain = 10 * time.Minute
	// archiveBatch bounds the archives owed one doctor's run asks for: one
	// steward's turn, archiveEach per session.
	archiveBatch = 20
)

// owe records in st the outcomes of the archives asked for at now: one
// left to do is owed (a steward asked in vain counts a try), one done or
// never to be done is owed no longer, and one past its bounds is given up.
// It returns each outcome's line saying so.
func owe(st *state.State, outcomes []archiveOutcome, now time.Time) []string {
	lines := make([]string, len(outcomes))
	for i, o := range outcomes {
		lines[i] = o.line
		j := slices.IndexFunc(st.Archives, func(x state.Archive) bool { return x.Is(o.agent) })
		if j >= 0 {
			lines[i] += fmt.Sprintf(" (owed since %s)", clock(now, st.Archives[j].Since))
		}
		if o.host == "" {
			if j >= 0 {
				st.Archives = slices.Delete(st.Archives, j, j+1)
			}
			continue
		}
		if j < 0 {
			st.Archives = append(st.Archives, state.Archive{Party: o.agent, Host: o.host, Since: now})
			j = len(st.Archives) - 1
		}
		ar := &st.Archives[j]
		ar.Why = o.line
		if o.asked {
			ar.Tries++
			ar.Tried = now
		}
		if why := givenUp(*ar, now); why != "" {
			st.Archives = slices.Delete(st.Archives, j, j+1)
			lines[i] += "; the doctor gives it up: " + why
			continue
		}
		lines[i] += "; the doctor asks again"
	}
	return lines
}

// givenUp says why the archive ar is owed no longer at now, "" while it is.
func givenUp(ar state.Archive, now time.Time) string {
	switch {
	case ar.Tries >= archiveTries:
		return fmt.Sprintf("%d stewards' turns did not archive it", ar.Tries)
	case now.Sub(ar.Since) >= archiveOwedFor:
		return "owed " + dur(now.Sub(ar.Since))
	}
	return ""
}

// seedArchives owes, once, the archives of the finished workers whose
// desktop records stayed unarchived: a session beekeeper started, off the
// roster, holding no role, its record unarchived, its CLI running or not.
// The doctor asks for at most archiveBatch of them per run. A state seeded
// before a session's own warmed CLI archived it is seeded once more, for
// the archives given up while no idle steward ran: only the starts reseed
// takes, never a session someone brought back from the Archived list, each
// owed from when its session has stayed quiet reseedQuiet (reseedWhy).
func seedArchives(st *state.State, record func(host string) (*claude.Record, bool), reseed func(*state.State, state.Start) (time.Time, bool), now time.Time) {
	if st.FinishedSeeded && st.DoneSeeded {
		return
	}
	again := st.FinishedSeeded
	st.ArchivesSeeded, st.FinishedSeeded, st.DoneSeeded = true, true, true
	for _, s := range st.Starts {
		switch {
		case s.HostSession == "" || s.Harness != "" || roleKeeps(st, s.Party),
			slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(s.Party) }),
			slices.ContainsFunc(st.Archives, func(x state.Archive) bool { return x.Host == s.HostSession }):
			continue
		}
		r, ok := record(s.HostSession)
		if !ok || r.IsArchived {
			continue
		}
		if !again {
			st.Archives = append(st.Archives, state.Archive{Party: s.Party, Host: s.HostSession, Since: now, Why: "a finished worker left unarchived"})
			continue
		}
		if r.AutoArchiveExempt {
			continue
		}
		if active, ok := reseed(st, s); ok {
			since := now
			if quiet := latest(active, r.LastSeen()).Add(reseedQuiet); quiet.After(since) {
				since = quiet
			}
			st.Archives = append(st.Archives, state.Archive{Party: s.Party, Host: s.HostSession, Since: since.UTC(), Why: reseedWhy})
		}
	}
}

// reseedWhy is why the reseed owes an archive: the doctor asks for none
// while its session ran a turn or was shown within reseedQuiet.
const reseedWhy = "a finished worker left unarchived, owed once its session stays quiet a day"

// reseedQuiet is how long a reseeded session stays without a turn and
// unshown before the doctor asks for its archive.
const reseedQuiet = 24 * time.Hour

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// reseedable says whether the reseed owes the archive of start s, and when
// its transcript was last written: an agent that reported its work done
// (agents.done) or a role's run a relay relieved, whose CLI does not run and
// whose session no person typed in. done holds the sessions that reported
// done; transcript finds a session's transcript.
func reseedable(st *state.State, s state.Start, done map[string]bool, live func(state.Party) bool,
	transcript func(id string) (string, bool),
) (time.Time, bool) {
	if !done[s.Session] && !relieved(st, s.Party) || live(s.Party) {
		return time.Time{}, false
	}
	path, ok := transcript(s.Session)
	if !ok {
		return time.Time{}, false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	typed, err := claude.PersonTyped(path)
	return fi.ModTime(), err == nil && !typed
}

// owedArchive is what the doctor does with one archive owed: ask for it
// again, wait (wait says why), or owe it no longer (drop says why).
type owedArchive struct {
	ar         state.Archive
	wait, drop string
	now        time.Time
}

// String says it as the doctor would do it.
func (o owedArchive) String() string {
	switch {
	case o.drop != "":
		return fmt.Sprintf("owe %q no longer the archive of its desktop session %s: %s", o.ar.Name, o.ar.Host, o.drop)
	case o.wait != "":
		return fmt.Sprintf("owe %q the archive of its desktop session %s, waiting: %s", o.ar.Name, o.ar.Host, o.wait)
	}
	why, _, _ := strings.Cut(o.ar.Why, "\n")
	return fmt.Sprintf("ask again to archive the desktop session %s of %q (try %d of %d, owed since %s: %s)",
		o.ar.Host, o.ar.Name, o.ar.Tries+1, archiveTries, clock(o.now, o.ar.Since), why)
}

// planArchives sorts the archives st owes at now. One is owed no longer once
// the desktop has it archived or no session of it, the agent is on the
// roster again or keeps a role unrelieved, or it is past its bounds; it waits while its
// CLI is busy, or archiveAgain after a steward's turn did not record it.
// record reads a desktop session's record.
func planArchives(st *state.State, record func(host string) (*claude.Record, bool), busy func(state.Party) bool, now time.Time) []owedArchive {
	out := make([]owedArchive, 0, len(st.Archives))
	for _, ar := range st.Archives {
		o := owedArchive{ar: ar, now: now}
		r, ok := record(ar.Host)
		switch {
		case !ok:
			o.drop = "the desktop has no session of it"
		case r.IsArchived:
			o.drop = "the desktop has it archived"
		case slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(ar.Party) }):
			o.drop = "it is on the roster again"
		case roleKeeps(st, ar.Party):
			o.drop = "it holds or held the supervisor's or the guide's role, and no relay relieved it"
		case givenUp(ar, now) != "":
			o.drop = "the doctor gives it up: " + givenUp(ar, now)
		case busy(ar.Party):
			o.wait = "its CLI runs a turn or a gated merge"
		case ar.Why == reseedWhy && now.Before(latest(ar.Since, r.LastSeen().Add(reseedQuiet))):
			o.wait = fmt.Sprintf("its session stays quiet a day first, until %s", clock(now, latest(ar.Since, r.LastSeen().Add(reseedQuiet))))
		case !ar.Tried.IsZero() && now.Sub(ar.Tried) < archiveAgain:
			o.wait = fmt.Sprintf("a steward was asked %s ago", dur(now.Sub(ar.Tried)))
		}
		out = append(out, o)
	}
	return out
}

// oweArchive owes from now the archive of the desktop session beekeeper
// started for p, which a hand-over or a relay left behind: the doctor asks
// a steward for it while its CLI runs no turn. It returns the session, ""
// when beekeeper started none for p (a person's session, an omp agent).
func oweArchive(st *state.State, p state.Party, why string, now time.Time) string {
	i := slices.IndexFunc(st.Starts, func(x state.Start) bool { return x.Session != "" && x.Session == p.Session })
	if i < 0 || st.Starts[i].HostSession == "" || st.Starts[i].Harness != "" {
		return ""
	}
	s := st.Starts[i]
	if !slices.ContainsFunc(st.Archives, func(x state.Archive) bool { return x.Host == s.HostSession }) {
		st.Archives = append(st.Archives, state.Archive{Party: s.Party, Host: s.HostSession, Since: now.UTC(), Why: why})
	}
	return s.HostSession
}

// reseed is the doctor's reseedable on the running sessions: it reads the
// sessions that reported done from the event log once, when first asked.
func (a *app) reseed(sessions []*claude.Session) func(*state.State, state.Start) (time.Time, bool) {
	var done map[string]bool
	live := func(p state.Party) bool { _, ok := claude.Live(sessions, p); return ok }
	transcript := func(id string) (string, bool) {
		m, _ := filepath.Glob(filepath.Join(a.cfg.Claude.ProjectsDir, "*", id+".jsonl"))
		if len(m) == 0 {
			return "", false
		}
		return m[0], true
	}
	return func(st *state.State, s state.Start) (time.Time, bool) {
		if done == nil {
			done = map[string]bool{}
			evs, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == "agents.done" })
			for _, e := range evs {
				done[e.By.Session] = true
			}
		}
		return reseedable(st, s, done, live, transcript)
	}
}
