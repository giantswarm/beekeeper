package cmd

import (
	"fmt"
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
// turns and archiveOwedFor.
const (
	archiveTries   = 5
	archiveOwedFor = 24 * time.Hour
	// archiveAgain is how long the doctor leaves an archive a steward did
	// not record before it asks again.
	archiveAgain = 10 * time.Minute
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

// seedArchives owes, once, the archives of the finished workers left with
// a running CLI before the doctor kept owed archives: a session beekeeper
// started, off the roster, holding no role, its CLI running and its desktop
// record unarchived.
func seedArchives(st *state.State, sessions []*claude.Session, record func(host string) (*claude.Record, bool), now time.Time) {
	if st.ArchivesSeeded {
		return
	}
	st.ArchivesSeeded = true
	for _, s := range st.Starts {
		switch {
		case s.HostSession == "" || s.Harness != "" || keepsRole(st, s.Party),
			slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(s.Party) }),
			slices.ContainsFunc(st.Archives, func(x state.Archive) bool { return x.Host == s.HostSession }):
			continue
		}
		if _, live := claude.Live(sessions, s.Party); !live {
			continue
		}
		if r, ok := record(s.HostSession); ok && !r.IsArchived {
			st.Archives = append(st.Archives, state.Archive{Party: s.Party, Host: s.HostSession, Since: now, Why: "its CLI ran on off the roster"})
		}
	}
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
// roster again or keeps a role, or it is past its bounds; it waits while its
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
		case keepsRole(st, ar.Party):
			o.drop = "it holds or held the supervisor's or the guide's role"
		case givenUp(ar, now) != "":
			o.drop = "the doctor gives it up: " + givenUp(ar, now)
		case busy(ar.Party):
			o.wait = "its CLI runs a turn or a gated merge"
		case !ar.Tried.IsZero() && now.Sub(ar.Tried) < archiveAgain:
			o.wait = fmt.Sprintf("a steward was asked %s ago", dur(now.Sub(ar.Tried)))
		}
		out = append(out, o)
	}
	return out
}
