package watch

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// NoteKept is the event of a note the watch keeps open although every
// issue and pull request it asks about is closed or merged: a closing
// keyword closed one, or the worker that filed it still runs its task.
// Its detail is "#<id> kept: <reason>; the note: <text>".
const NoteKept = "note.kept"

// overtake is a note the watch closes as overtaken, and why.
type Overtake struct {
	Id     int
	Reason string
}

// refsSettle reports whether n's refs close it once each is closed or
// merged: an overtakable note with refs, except a person's to-do, which is
// theirs whatever happens to the issue it names, and a note reopened by
// hand, which said its settled refs do not settle it. Only an answer, a
// default or a close by hand ends either.
func refsSettle(n state.Note, asking int) bool {
	return Cmd.Overtakable(n, asking) && len(n.Refs) > 0 && n.Kind != noteTodo && n.Reopened.IsZero()
}

// OvertakeRefs are the distinct issues and pull requests of the notes of
// st whose refs settle them.
func OvertakeRefs(st *state.State) []github.PR {
	var out []github.PR
	asking := st.GuideRole().LocalAsking()
	for _, n := range st.Notes {
		if !refsSettle(n, asking) {
			continue
		}
		for _, s := range n.Refs {
			if r, ok := Cmd.ParseRef(s); ok && !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	}
	return out
}

// settledRefs says how refs are settled once every one is closed or merged
// ("o/r#1 closed by o/r#2's closing keyword, o/r#3 merged"; states as
// GitHub answered) and whether a closing keyword closed one of them; ""
// while any is open or unanswered.
func settledRefs(refs []string, states map[github.PR]github.RefState) (string, bool) {
	var parts []string
	var keyword bool
	for _, s := range refs {
		r, ok := Cmd.ParseRef(s)
		at := states[r]
		if !ok || at.State == "" || at.State == github.Open {
			return "", false
		}
		part := Cmd.RefName(r) + " " + strings.ToLower(at.State)
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
// nobody's. A PR already merged when the note was added keeps it open:
// that merge cannot settle a question about the merged PR.
// A note without refs is never overtaken: its filing session
// archived says the asker is gone, not that the question is settled
// (findOrphaned). Nor is a to-do or a reopened note (refsSettle).
func findOvertaken(st *state.State, states map[github.PR]github.RefState) (over, kept []Overtake) {
	asking := st.GuideRole().LocalAsking()
	for _, n := range st.Notes {
		if !refsSettle(n, asking) {
			continue
		}
		settled, keyword := settledRefs(n.Refs, states)
		if settled == "" {
			continue
		}
		// A merge that predates the question cannot answer it, even when
		// the filing worker has since finished its task.
		alreadyMerged := false
		for _, ref := range n.Refs {
			r, _ := Cmd.ParseRef(ref)
			at := states[r]
			if at.State == github.Merged && !at.MergedAt.IsZero() && !n.At.IsZero() && !at.MergedAt.After(n.At) {
				kept = append(kept, Overtake{n.ID, Cmd.RefName(r) + " already merged when the note was added"})
				alreadyMerged = true
				break
			}
		}
		if alreadyMerged {
			continue
		}
		// The task's end is recorded first: a worker on its next task still
		// runs, its earlier notes are settled.
		switch w := runningWorker(st, n); {
		case !n.TaskEnded.IsZero():
			over = append(over, Overtake{n.ID, fmt.Sprintf("%s, its worker %q done", settled, n.By.Name)})
		case w != nil:
			kept = append(kept, Overtake{n.ID, fmt.Sprintf("%s, its worker %q still runs", settled, w.Name)})
		case keyword:
			kept = append(kept, Overtake{n.ID, settled + ", not by its worker"})
		default:
			over = append(over, Overtake{n.ID, settled})
		}
	}
	return over, kept
}

// overtakenNow reads what findOvertaken needs, GitHub only for notes with
// refs and while the budget is over its floor, and returns the overtaken
// notes and the kept ones. A failed read is a NOTE line (fail); --once also
// says the read it skips for the budget and the refs GitHub left
// unanswered, which keep their notes open: its silence means no change.
func (w *Watcher) overtakenNow(ctx context.Context, st *state.State) (over, kept []Overtake) {
	var states map[github.PR]github.RefState
	refs := OvertakeRefs(st)
	switch {
	case len(refs) == 0:
	case Cmd.LowBudget(st.Budget, w.Host.Cfg().GitHub.Floor, w.Host.Now()):
		if !w.Chores {
			w.EmitNow("pending", "NOTE REFS NOT READ (--once): the GitHub budget is under its floor until %s; no note is overtaken or kept",
				st.Budget.Reset.Local().Format("15:04"))
		}
	default:
		// A failed batch keeps its notes open; the others' are decided.
		s, err := Cmd.RefStates(ctx, refs)
		states = s
		if err != nil {
			w.Fail("note-refs", "NOTE REFS UNREADABLE: cannot read the notes' issues and pull requests: %v", err)
			break
		}
		w.Clear("note-refs")
		if unanswered := slices.DeleteFunc(slices.Clone(refs), func(r github.PR) bool { _, ok := s[r]; return ok }); len(unanswered) > 0 && !w.Chores {
			names := make([]string, len(unanswered))
			for i, r := range unanswered {
				names[i] = Cmd.RefName(r)
			}
			w.EmitNow("pending", "NOTE REFS UNANSWERED (--once): %s; their notes stay open", strings.Join(names, ", "))
		}
	}
	return findOvertaken(st, states)
}

// keepOpen records on the open notes of st among kept why the watch keeps
// them open and returns the lines and events of the reasons it has not
// said yet: a note says each reason once.
func keepOpen(st *state.State, kept []Overtake, by state.Party) ([]string, []state.Event) {
	var lines []string
	var evs []state.Event
	for i := range st.Notes {
		n := &st.Notes[i]
		j := slices.IndexFunc(kept, func(o Overtake) bool { return o.Id == n.ID })
		if j < 0 || n.Kept == kept[j].Reason {
			continue
		}
		n.Kept = kept[j].Reason
		lines = append(lines, fmt.Sprintf("NOTE KEPT: #%d, %s: %s", n.ID, n.Kept, Cmd.Truncate(n.Text, 200)))
		evs = append(evs, Cmd.Event(by, NoteKept, "#%d kept: %s%s%s", n.ID, Cmd.OneLine(n.Kept), overtakenSep, n.Text))
	}
	return lines, evs
}

// closeOvertaken closes the open notes of st among over as overtaken and
// returns their watch lines and events. A note no longer open, answered or
// done meanwhile, is left alone.
func closeOvertaken(st *state.State, over []Overtake, by state.Party) ([]string, []state.Event) {
	var lines []string
	var evs []state.Event
	st.Notes = slices.DeleteFunc(st.Notes, func(n state.Note) bool {
		i := slices.IndexFunc(over, func(o Overtake) bool { return o.Id == n.ID })
		if i < 0 {
			return false
		}
		lines = append(lines, fmt.Sprintf("NOTE OVERTAKEN: #%d, %s: %s", n.ID, over[i].Reason, Cmd.Truncate(n.Text, 200)))
		evs = append(evs, Cmd.OvertakenEvent(by, n, over[i].Reason))
		return true
	})
	return lines, evs
}

// heldAmong are the notes among notes that over closes and that hold a
// board item, for their release.
func heldAmong(notes []state.Note, over []Overtake) []state.Note {
	var out []state.Note
	for _, n := range notes {
		if n.Held != nil && slices.ContainsFunc(over, func(o Overtake) bool { return o.Id == n.ID }) {
			out = append(out, n)
		}
	}
	return out
}

// wouldOvertake says, for a watch that writes nothing (--once), each note
// a running watch would close as overtaken, and each it would keep open
// for a reason it has not recorded yet.
func (w *Watcher) wouldOvertake(st *state.State, over, kept []Overtake) {
	for _, o := range over {
		i := slices.IndexFunc(st.Notes, func(n state.Note) bool { return n.ID == o.Id })
		if i >= 0 {
			w.EmitNow("pending", "NOTE OVERTAKEN (--once writes nothing): #%d, %s: %s", o.Id, o.Reason, Cmd.Truncate(st.Notes[i].Text, 200))
		}
	}
	for _, o := range kept {
		i := slices.IndexFunc(st.Notes, func(n state.Note) bool { return n.ID == o.Id })
		if i >= 0 && st.Notes[i].Kept != o.Reason {
			w.EmitNow("pending", "NOTE KEPT (--once writes nothing): #%d, %s: %s", o.Id, o.Reason, Cmd.Truncate(st.Notes[i].Text, 200))
		}
	}
}
