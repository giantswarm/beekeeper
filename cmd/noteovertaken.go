package cmd

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// noteOvertaken is the event of a note closed because what it asks about
// is settled: its issues and pull requests closed or merged, or a reason
// given by hand. Its detail is "#<id> overtaken: <reason>; the note: <text>".
const noteOvertaken = "note.overtaken"

// noteDone is the verb of a note marked done.
const noteDone = "note.done"

// overtakenSep separates an overtaken event's reason from the note's text.
const overtakenSep = "; the note: "

// refStates reads the states of issues and pull requests, batch by batch
// (github.RefStates): an error names the batches that failed, the states
// are the others'; a seam for the tests.
var refStates = func(ctx context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
	return github.RefStates(ctx, notesGH, refs)
}

// parseRef reads an issue or pull request as owner/repo#n or its full URL.
func parseRef(s string) (github.PR, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return github.PR{}, false
	}
	if strings.Contains(s, "://") {
		if m := refURL.FindStringSubmatch(s); m != nil && m[0] == s {
			n, _ := strconv.Atoi(m[2])
			return github.PR{Repo: m[1], N: n}, true
		}
		return github.PR{}, false
	}
	if m := ghRef.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[2])
		return github.PR{Repo: m[1], N: n}, true
	}
	return github.PR{}, false
}

// refName is ref as owner/repo#n.
func refName(r github.PR) string { return fmt.Sprintf("%s#%d", r.Repo, r.N) }

// overtakable reports whether the watch may close n as overtaken: a note
// for someone, not a login (its probe closes it), and not the note the guide asks its person now, whose
// answer would otherwise find it closed.
func overtakable(n state.Note, asking int) bool {
	return n.For != "" && n.Kind != noteLogin && n.ID != asking
}

// sessionLinked reports whether an overtakable note of st is linked to its
// filing session: it has no refs.
func sessionLinked(st *state.State) bool {
	asking := st.GuideRole().LocalAsking()
	return slices.ContainsFunc(st.Notes, func(n state.Note) bool { return overtakable(n, asking) && len(n.Refs) == 0 })
}

// roleRun reports whether p is or was a run of a role: the role, not the
// run, owns its notes, and a relay hands them on.
func roleRun(st *state.State, p state.Party) bool {
	for _, rl := range roles {
		r := rl.Get(st)
		if rl.RunOf(p.Name) > 0 || r.Holder != nil && r.Holder.Is(p) ||
			slices.ContainsFunc(r.Relieved, func(rf state.Relief) bool { return rf.Party.Is(p) }) {
			return true
		}
	}
	return false
}

// findOrphaned returns the open notes of st without refs whose filing
// session is archived (archived; stopped is not archived), the guide's
// candidates to ask or close by hand. A role's run never orphans its
// notes: the role owns them.
func findOrphaned(st *state.State, archived map[string]bool) []overtake {
	var out []overtake
	asking := st.GuideRole().LocalAsking()
	for _, n := range st.Notes {
		by := n.By
		if overtakable(n, asking) && len(n.Refs) == 0 && (archived[by.HostSession] || archived[by.Session]) && !roleRun(st, by) {
			out = append(out, overtake{Id: n.ID, Reason: fmt.Sprintf("its filing session %q is archived", by.Name)})
		}
	}
	return out
}

// overtakenEvent is the note.overtaken event of n closed for reason. It
// retains the whole note, which note show and note reopen read back from a
// file store's history.
func overtakenEvent(by state.Party, n state.Note, reason string) state.Event {
	e := event(by, noteOvertaken, "#%d overtaken: %s%s%s", n.ID, oneLine(reason), overtakenSep, n.Text)
	e.NoteSnapshot = &n
	return e
}

// overtakenReason is the reason an overtaken event records.
func overtakenReason(e state.Event) string {
	_, rest, _ := strings.Cut(e.Detail, " overtaken: ")
	reason, _, _ := strings.Cut(rest, overtakenSep)
	return reason
}

// overtakenNote is the note an overtaken event closed, with its reason: the
// event's snapshot, or the id and text alone of an event written before
// overtaken events retained the note, whose person is then unknown. nil for
// any other event.
func overtakenNote(e state.Event) *shownNote {
	if e.Verb != noteOvertaken {
		return nil
	}
	var n state.Note
	if e.NoteSnapshot != nil {
		n = *e.NoteSnapshot
	} else {
		head, text, ok := strings.Cut(e.Detail, overtakenSep)
		if _, err := fmt.Sscanf(head, "#%d overtaken:", &n.ID); err != nil || !ok {
			return nil
		}
		n.Text = text
	}
	return &shownNote{Note: n, Status: noteStatusClosed, Reason: overtakenReason(e), Closure: &e}
}
