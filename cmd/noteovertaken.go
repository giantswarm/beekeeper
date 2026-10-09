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

// noteKept is the event of a note the watch keeps open although every
// issue and pull request it asks about is closed or merged: a closing
// keyword closed one, or the worker that filed it still runs its task.
// Its detail is "#<id> kept: <reason>; the note: <text>".
const noteKept = "note.kept"

// overtakenSep separates an overtaken event's reason from the note's text.
const overtakenSep = "; the note: "

// refStates reads the states of issues and pull requests; a seam for the
// tests.
var refStates = func(ctx context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
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

// settledRefs says how refs are settled once every one is closed or merged
// ("o/r#1 closed by o/r#2's closing keyword, o/r#3 merged"; states as
// GitHub answered) and whether a closing keyword closed one of them; ""
// while any is open or unanswered.
func settledRefs(refs []string, states map[github.PR]github.RefState) (string, bool) {
	var parts []string
	var keyword bool
	for _, s := range refs {
		r, ok := parseRef(s)
		at := states[r]
		if !ok || at.State == "" || at.State == github.Open {
			return "", false
		}
		part := refName(r) + " " + strings.ToLower(at.State)
		if at.Closer != "" {
			part += " by " + at.Closer + "'s closing keyword"
			keyword = true
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", "), keyword
}

// runningWorker returns the agent of st that filed n while its task still
// runs (busy or parked, not reported idle or done); nil for a note a role,
// a person's own session or a finished worker filed.
func runningWorker(st *state.State, n state.Note) *state.Agent {
	for i := range st.Agents {
		if ag := &st.Agents[i]; ag.Is(n.By) && ag.Task != "" && !ag.Done {
			return ag
		}
	}
	return nil
}

// findOvertaken returns the open notes of st that are overtaken, and the
// ones kept open although every ref is settled. A note with refs is
// overtaken once every one of them is closed or merged (an unanswered ref
// keeps the note open) when its worker closed them at the end of its task:
// the worker that filed it has reported its task over (TaskEnded), or no
// closing keyword closed one and no worker runs for it. While the filing
// worker still runs, or a pull request's closing keyword closed a ref
// before any worker's task ended, the note is kept: the decision is still
// nobody's. A note without refs is never overtaken: its filing session
// archived says the asker is gone, not that the question is settled
// (findOrphaned).
func findOvertaken(st *state.State, states map[github.PR]github.RefState) (over, kept []overtake) {
	asking := st.GuideRole().Asking
	for _, n := range st.Notes {
		if !overtakable(n, asking) || len(n.Refs) == 0 {
			continue
		}
		settled, keyword := settledRefs(n.Refs, states)
		if settled == "" {
			continue
		}
		// The task's end is recorded first: a worker on its next task still
		// runs, its earlier notes are settled.
		switch w := runningWorker(st, n); {
		case !n.TaskEnded.IsZero():
			over = append(over, overtake{n.ID, fmt.Sprintf("%s, its worker %q done", settled, n.By.Name)})
		case w != nil:
			kept = append(kept, overtake{n.ID, fmt.Sprintf("%s, its worker %q still runs", settled, w.Name)})
		case keyword:
			kept = append(kept, overtake{n.ID, settled + ", not by its worker"})
		default:
			over = append(over, overtake{n.ID, settled})
		}
	}
	return over, kept
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
// notes and the kept ones. A failed read is a NOTE line (fail); --once also
// says the read it skips for the budget and the refs GitHub left
// unanswered, which keep their notes open: its silence means no change.
func (w *watcher) overtakenNow(ctx context.Context, st *state.State) (over, kept []overtake) {
	var states map[github.PR]github.RefState
	refs := overtakeRefs(st)
	switch {
	case len(refs) == 0:
	case lowBudget(st.Budget, w.cfg.GitHub.Floor, w.now):
		if !w.chores {
			w.emitNow("pending", "NOTE REFS NOT READ (--once): the GitHub budget is under its floor until %s; no note is overtaken or kept",
				st.Budget.Reset.Local().Format("15:04"))
		}
	default:
		s, err := refStates(ctx, refs)
		if s == nil {
			w.fail("note-refs", "NOTE REFS UNREADABLE: cannot read the notes' issues and pull requests: %v", err)
			break
		}
		w.clear("note-refs")
		states = s
		if unanswered := slices.DeleteFunc(slices.Clone(refs), func(r github.PR) bool { _, ok := s[r]; return ok }); len(unanswered) > 0 && !w.chores {
			names := make([]string, len(unanswered))
			for i, r := range unanswered {
				names[i] = refName(r)
			}
			w.emitNow("pending", "NOTE REFS UNANSWERED (--once): %s; their notes stay open", strings.Join(names, ", "))
		}
	}
	return findOvertaken(st, states)
}

// keepOpen records on the open notes of st among kept why the watch keeps
// them open and returns the lines and events of the reasons it has not
// said yet: a note says each reason once.
func keepOpen(st *state.State, kept []overtake, by state.Party) ([]string, []state.Event) {
	var lines []string
	var evs []state.Event
	for i := range st.Notes {
		n := &st.Notes[i]
		j := slices.IndexFunc(kept, func(o overtake) bool { return o.id == n.ID })
		if j < 0 || n.Kept == kept[j].reason {
			continue
		}
		n.Kept = kept[j].reason
		lines = append(lines, fmt.Sprintf("NOTE KEPT: #%d, %s: %s", n.ID, n.Kept, truncate(n.Text, 200)))
		evs = append(evs, event(by, noteKept, "#%d kept: %s%s%s", n.ID, oneLine(n.Kept), overtakenSep, n.Text))
	}
	return lines, evs
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
// a running watch would close as overtaken, and each it would keep open
// for a reason it has not recorded yet.
func (w *watcher) wouldOvertake(st *state.State, over, kept []overtake) {
	for _, o := range over {
		i := slices.IndexFunc(st.Notes, func(n state.Note) bool { return n.ID == o.id })
		if i >= 0 {
			w.emitNow("pending", "NOTE OVERTAKEN (--once writes nothing): #%d, %s: %s", o.id, o.reason, truncate(st.Notes[i].Text, 200))
		}
	}
	for _, o := range kept {
		i := slices.IndexFunc(st.Notes, func(n state.Note) bool { return n.ID == o.id })
		if i >= 0 && st.Notes[i].Kept != o.reason {
			w.emitNow("pending", "NOTE KEPT (--once writes nothing): #%d, %s: %s", o.id, o.reason, truncate(st.Notes[i].Text, 200))
		}
	}
}
