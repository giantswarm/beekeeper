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

// overtakenSep separates an overtaken event's reason from the note's text.
const overtakenSep = "; the note: "

// refStates reads the states of issues and pull requests; a seam for the
// tests.
var refStates = func(ctx context.Context, refs []github.PR) (map[github.PR]string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return github.RefStates(ctx, notesGH, refs)
}

// overtake is a note the watch closes as overtaken, and why.
type overtake struct {
	id     int
	reason string
}

// parseRef reads an issue or pull request as owner/repo#n or its full URL.
func parseRef(s string) (github.PR, bool) {
	s = strings.TrimSpace(s)
	if m := refURL.FindStringSubmatch(s); m != nil && m[0] == s {
		n, _ := strconv.Atoi(m[2])
		return github.PR{Repo: m[1], N: n}, true
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
// for someone, neither pinned (a standing instruction) nor a login (its
// probe closes it), and not the note the guide asks its person now, whose
// answer would otherwise find it closed.
func overtakable(n state.Note, asking int) bool {
	return n.For != "" && !n.Pinned && n.Kind != noteLogin && n.ID != asking
}

// overtakeRefs are the distinct issues and pull requests the overtakable
// notes of st are linked to.
func overtakeRefs(st *state.State) []github.PR {
	var out []github.PR
	asking := st.GuideRole().Asking
	for _, n := range st.Notes {
		if !overtakable(n, asking) {
			continue
		}
		for _, s := range n.Refs {
			if r, ok := parseRef(s); ok && !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	}
	return out
}

// sessionLinked reports whether an overtakable note of st is linked to its
// filing session: it has no refs.
func sessionLinked(st *state.State) bool {
	asking := st.GuideRole().Asking
	return slices.ContainsFunc(st.Notes, func(n state.Note) bool { return overtakable(n, asking) && len(n.Refs) == 0 })
}

// roleRun reports whether p is or was a run of a role: the role, not the
// run, owns its notes, and a relay hands them on.
func roleRun(st *state.State, p state.Party) bool {
	for _, rl := range roles {
		r := rl.get(st)
		if rl.runOf(p.Name) > 0 || r.Holder != nil && r.Holder.Is(p) ||
			slices.ContainsFunc(r.Relieved, func(rf state.Relief) bool { return rf.Party.Is(p) }) {
			return true
		}
	}
	return false
}

// findOvertaken returns the open notes of st that are overtaken: a note
// with refs once every one of them is closed or merged (states, as GitHub
// answered; an unanswered ref keeps the note open). A note without refs is
// never overtaken: its filing session archived says the asker is gone, not
// that the question is settled (findOrphaned).
func findOvertaken(st *state.State, states map[github.PR]string) []overtake {
	var out []overtake
	asking := st.GuideRole().Asking
	for _, n := range st.Notes {
		if !overtakable(n, asking) || len(n.Refs) == 0 {
			continue
		}
		var settled []string
		for _, s := range n.Refs {
			r, ok := parseRef(s)
			at := states[r]
			if !ok || at == "" || at == github.Open {
				settled = nil
				break
			}
			settled = append(settled, refName(r)+" "+strings.ToLower(at))
		}
		if len(settled) > 0 {
			out = append(out, overtake{n.ID, strings.Join(settled, ", ")})
		}
	}
	return out
}

// findOrphaned returns the open notes of st without refs whose filing
// session is archived (archived; stopped is not archived), the guide's
// candidates to ask or close by hand. A role's run never orphans its
// notes: the role owns them.
func findOrphaned(st *state.State, archived map[string]bool) []overtake {
	var out []overtake
	asking := st.GuideRole().Asking
	for _, n := range st.Notes {
		by := n.By
		if overtakable(n, asking) && len(n.Refs) == 0 && (archived[by.HostSession] || archived[by.Session]) && !roleRun(st, by) {
			out = append(out, overtake{n.ID, fmt.Sprintf("its filing session %q is archived", by.Name)})
		}
	}
	return out
}

// overtakenNow reads what findOvertaken needs, GitHub only for notes with
// refs and while the budget is over its floor, and returns the overtaken
// notes.
func (w *watcher) overtakenNow(ctx context.Context, st *state.State) []overtake {
	var states map[github.PR]string
	if refs := overtakeRefs(st); len(refs) > 0 && !lowBudget(st.Budget, w.cfg.GitHub.Floor, w.now) {
		s, err := refStates(ctx, refs)
		w.check("note-refs", s == nil, "cannot read the notes' issues and pull requests: %v", err)
		states = s
	}
	return findOvertaken(st, states)
}

// closeOvertaken closes the open notes of st among over as overtaken and
// returns their watch lines and events. A note no longer open, answered or
// done meanwhile, is left alone.
func closeOvertaken(st *state.State, over []overtake, by state.Party) ([]string, []state.Event) {
	var lines []string
	var evs []state.Event
	st.Notes = slices.DeleteFunc(st.Notes, func(n state.Note) bool {
		i := slices.IndexFunc(over, func(o overtake) bool { return o.id == n.ID })
		if i < 0 {
			return false
		}
		lines = append(lines, fmt.Sprintf("NOTE OVERTAKEN: #%d, %s: %s", n.ID, over[i].reason, truncate(n.Text, 200)))
		evs = append(evs, overtakenEvent(by, n, over[i].reason))
		return true
	})
	return lines, evs
}

// overtakenEvent is the note.overtaken event of n closed for reason.
func overtakenEvent(by state.Party, n state.Note, reason string) state.Event {
	return event(by, noteOvertaken, "#%d overtaken: %s%s%s", n.ID, oneLine(reason), overtakenSep, n.Text)
}

// overtakenReason is the reason an overtaken event records.
func overtakenReason(e state.Event) string {
	_, rest, _ := strings.Cut(e.Detail, " overtaken: ")
	reason, _, _ := strings.Cut(rest, overtakenSep)
	return reason
}

// wouldOvertake says, for a watch that writes nothing (--once), each note
// a running watch would close as overtaken.
func (w *watcher) wouldOvertake(st *state.State, over []overtake) {
	for _, o := range over {
		i := slices.IndexFunc(st.Notes, func(n state.Note) bool { return n.ID == o.id })
		if i >= 0 {
			w.emitNow("pending", "NOTE OVERTAKEN (--once writes nothing): #%d, %s: %s", o.id, o.reason, truncate(st.Notes[i].Text, 200))
		}
	}
}
