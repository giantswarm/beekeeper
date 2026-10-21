package cmd

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// An archive owed is the desktop session of an agent that left the roster
// unarchived. The doctor puts the archives owed to the person as one
// decision through the guide (askArchives); on their yes the guide's
// session, the one they type in, archives each, and the doctor owes one no
// longer once the desktop records it. A decision closed while the desktop
// has one unarchived keeps that session. An archive no decision asked
// about, guide.person unset say, is given up after archiveOwedFor.
const (
	archiveOwedFor = 24 * time.Hour
	// archiveBatch bounds the archives one decision asks about.
	archiveBatch = 20
)

// owe records in st the outcomes of the archives taken off the roster at
// now: one left to do is owed, one done or never to be done is owed no
// longer, and one past its bounds is given up. It returns each outcome's
// line saying so.
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
		if why := givenUp(*ar, now); why != "" {
			st.Archives = slices.Delete(st.Archives, j, j+1)
			lines[i] += "; the doctor gives it up: " + why
		}
	}
	return lines
}

// givenUp says why the archive ar is owed no longer at now, "" while it is:
// owed archiveOwedFor with no decision asking the person about it.
func givenUp(ar state.Archive, now time.Time) string {
	if ar.Asked == 0 && now.Sub(ar.Since) >= archiveOwedFor {
		return "owed " + dur(now.Sub(ar.Since)) + " with no decision asking the person"
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

// owedArchive is what the doctor does with one archive owed: ask the
// person for it, wait (wait says why), or owe it no longer (drop says why).
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
	return fmt.Sprintf("ask the person to archive the desktop session %s of %q (owed since %s: %s)",
		o.ar.Host, o.ar.Name, clock(o.now, o.ar.Since), why)
}

// planArchives sorts the archives st owes at now. One is owed no longer once
// the desktop has it archived or no session of it, the agent is on the
// roster again or keeps a role unrelieved, the decision that asked about it
// closed (the person kept it), or it is past its bounds; it waits while a
// decision asks the person about it, or while its CLI is busy.
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
		case ar.Asked != 0 && !slices.ContainsFunc(st.Notes, func(n state.Note) bool { return n.ID == ar.Asked }):
			o.drop = fmt.Sprintf("the person's decision, note #%d, closed with it unarchived: it stays", ar.Asked)
		case givenUp(ar, now) != "":
			o.drop = "the doctor gives it up: " + givenUp(ar, now)
		case ar.Asked != 0:
			o.wait = fmt.Sprintf("note #%d asks the person", ar.Asked)
		case busy(ar.Party):
			o.wait = "its CLI runs a turn or a gated merge"
		case ar.Why == reseedWhy && now.Before(latest(ar.Since, r.LastSeen().Add(reseedQuiet))):
			o.wait = fmt.Sprintf("its session stays quiet a day first, until %s", clock(now, latest(ar.Since, r.LastSeen().Add(reseedQuiet))))
		}
		out = append(out, o)
	}
	return out
}

// archiveQuestion opens the question of the doctor's decision on the
// archives owed, which tells it from other notes.
const archiveQuestion = "Archive the desktop sessions of "

// isArchiveDecision reports whether n is the doctor's decision on archives.
func isArchiveDecision(n state.Note) bool {
	return n.By.Is(watchParty) && strings.HasPrefix(n.Question, archiveQuestion)
}

// askable are the archives st owes of the agents ask that no decision
// asked about and whose record still shows them archivable, at most
// archiveBatch, by their index in st.Archives.
func (a *app) askable(st *state.State, ask []state.Party) []int {
	var out []int
	for i, ar := range st.Archives {
		if len(out) == archiveBatch {
			break
		}
		if ar.Asked == 0 && slices.ContainsFunc(ask, ar.Is) {
			if _, why := a.archivableRecord(st, ar.Party); why == "" {
				out = append(out, i)
			}
		}
	}
	return out
}

// spentDecision reports whether n is an archive decision no archive owed
// names any more: the desktop recorded each, or the person's decision kept
// it.
func spentDecision(st *state.State, n state.Note) bool {
	return isArchiveDecision(n) && !slices.ContainsFunc(st.Archives, func(ar state.Archive) bool { return ar.Asked == n.ID })
}

// archiveDecisionDue reports whether askArchives changes st: a spent
// decision to close, or, with person set and no decision open, an archive
// of ask to put to them. The doctor writes the state only then.
func (a *app) archiveDecisionDue(st *state.State, ask []state.Party, person string) bool {
	if slices.ContainsFunc(st.Notes, func(n state.Note) bool { return spentDecision(st, n) }) {
		return true
	}
	return person != "" && !slices.ContainsFunc(st.Notes, isArchiveDecision) && len(a.askable(st, ask)) > 0
}

// askArchives closes the doctor's spent archive decision, and puts the
// archives of ask that are askable to person as one decision for the
// guide while none is open. On the person's yes the guide's session
// confirms them with agents archivable and archives each itself; nothing
// else archives them. The note is the watch's, whoever runs the doctor (by,
// in the events). It returns the lines and events of what it filed and
// closed.
func (a *app) askArchives(st *state.State, ask []state.Party, person string, by state.Party, now time.Time) (lines []string, evs []state.Event) {
	st.Notes = slices.DeleteFunc(st.Notes, func(n state.Note) bool {
		if !spentDecision(st, n) {
			return false
		}
		const why = "the doctor owes none of its archives any more"
		lines = append(lines, fmt.Sprintf("closed note #%d: %s", n.ID, why))
		evs = append(evs, overtakenEvent(by, n, why))
		return true
	})
	if person == "" || slices.ContainsFunc(st.Notes, isArchiveDecision) {
		return lines, evs
	}
	idx := a.askable(st, ask)
	if len(idx) == 0 {
		return lines, evs
	}
	owed := make([]*state.Archive, len(idx))
	for i, j := range idx {
		owed[i] = &st.Archives[j]
	}
	st.NextNote++
	id := st.NextNote
	hosts := make([]string, len(owed))
	listed := make([]string, len(owed))
	for i, ar := range owed {
		ar.Asked = id
		hosts[i] = ar.Host
		listed[i] = fmt.Sprintf("%q (%s)", ar.Name, ar.Host)
	}
	d := archiveDraft(id, hosts, listed)
	n := state.Note{ID: id, For: person, Text: d.text(), By: watchParty, At: now.UTC(), Kind: noteDecision,
		Question: d.Question, StatusQuo: d.StatusQuo, Options: d.Options, Recommend: d.Recommend,
		Default: "the sessions stay in the sidebar until the person answers"}
	st.Notes = append(st.Notes, n)
	lines = append(lines, fmt.Sprintf("note #%d for %s: archive %d finished workers' desktop sessions", id, person, len(owed)))
	evs = append(evs, event(by, "note.add", "#%d %s", n.ID, n.Text))
	return lines, evs
}

// archiveDraft is the decision of note id on archiving the desktop sessions
// hosts, listed by name: on a yes the guide's session confirms and archives
// them, on a no it closes the note and the sessions stay.
func archiveDraft(id int, hosts, listed []string) noteDraft {
	ids := strings.Join(hosts, " ")
	return noteDraft{
		Question: fmt.Sprintf("%s%d finished workers beekeeper started?", archiveQuestion, len(hosts)),
		StatusQuo: fmt.Sprintf("these desktop sessions of workers beekeeper started, finished or relieved of a role, stay in the sidebar: %s. "+
			"A session the person started is never listed; archiving is reversible (the desktop's Archived list brings one back)", strings.Join(listed, ", ")),
		Why: "the doctor archives no session without the person's yes, and an agreement relayed by another session is not that",
		Options: []string{
			fmt.Sprintf("archive them: this guide session runs `beekeeper agents archivable %s`, calls %s for each session_id it confirms, then `beekeeper note done %d`", ids, archiveTool, id),
			fmt.Sprintf("keep them: `beekeeper note done %d`; they stay in the sidebar and the doctor asks about them no more", id),
		},
		Recommend: 1,
	}
}

// oweArchive owes from now the archive of the desktop session beekeeper
// started for p, which a hand-over or a relay left behind: the doctor asks
// the person for it once its CLI runs no turn. It returns the session, ""
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
		path := transcriptOf(a.cfg, id)
		return path, path != ""
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
