package cmd

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/state"
)

// noteReplaced is the event of a note closed by the note that replaces it.
const noteReplaced = "note.replaced"

func (a *app) noteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "note",
		Short: "Open items that outlive a session: questions for a person, deadlines",
		Long: `Notes are the open items a supervisor would otherwise carry only in its
transcript. A decision waits on a person and carries its due time and its
default, what happens when nobody answers by then; only open decisions
reach the guide (guide queue, guide watch). A memo is a session's own
record, a state summary, a board skip, a list of deferred work: it asks
nobody, and a successor memo replaces its predecessor (add --replaces).
beekeeper watch reports a note once when it is due. Notes appear in every
hand-over, decisions and memos apart, until marked done. A pinned note (--pin) is a standing
instruction: every hand-over, the supervisor's and the guide's, carries it
in full until it is unpinned or done.

Without a subcommand, lists the open notes.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return a.noteList("") },
	}
	var forWho, due, overtaken, replaces string
	var pin bool
	var refs []string
	var draft noteDraft
	add := &cobra.Command{
		Use:   "add <text>",
		Short: "Add a note",
		Long: `Add a note. --kind is decision for the guide's person (guide.person; with
it unset, any --for) and memo otherwise. A decision names who decides
(--for) and is refused without --due and a --default that is an action. A
decision for the guide's person is refused, naming what it lacks, unless it asks something (a
question mark, an --option or a request verb opening it; a status line goes
to beekeeper log add) and carries what the person needs to answer without
asking back: --status-quo and --why, every
--option as "<choice>: <consequence>", a --default that is an action (not
"wait" or "none"), the full URL of every #N or owner/repo#N it names, and
--checked "<source>" for a claim that something is merged, green,
released, rolled or closed. A note that links an open pull request of a
plans repository (plans.repositories) is refused while that pull request's
stage check (plans.check, default plan-stages) is red, pending or missing,
naming what the check found. A checked note on an issue or PR that an open
note for the same person names, asking the same verb (the first word of
the text), folds into that note (note.folded). A --kind login note closes
once its --until probe, a shell command the watch runs every tick, exits 0.
A memo is not checked. A note for a person
that asks again what was answered for that person within the last 72
hours (the same verb on one of the same issues or PRs) is filed with a
warning that quotes the answer.

--ref links the note to an issue or pull request it asks about
(owner/repo#n or its URL, repeatable): once every linked one is closed or
merged, beekeeper watch closes the note as overtaken (note.overtaken),
when its worker closed them at the end of its task: an issue a pull
request's closing keyword closed at its merge, or any close while the
worker that filed the note still runs its task, keeps the note open
(NOTE KEPT, note.kept, once per reason); once the worker reports its task
over (agents idle), its settled issues overtake its notes.
A note without --ref stays open: once the session that filed it is
archived, guide watch names it to the guide as orphaned (GUIDE ORPHANED),
to ask or close by hand; a role's run never orphans its notes.

--replaces <id> closes the named open note in the same step (note.replaced)
and carries its pin over, so a state memo is one open note at a time.

A decision is put to its addressee as one message (beekeeper serve, through
klaus-gateway): --for a person, or --for team:<name> for any member of the
team. It is refused unless it renders: the question one line of at most 150
characters, --status-quo at most 3000, at most 10 --option with labels of at
most 75 characters, --recommend <n> naming one of them. At its due time
beekeeper watch closes it with its default (note.defaulted, NOTE DEFAULTED)
and tells the session that filed it.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := untilTime(a.now, due)
			if err != nil {
				return err
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			draft.Question = strings.Join(args, " ")
			linked := make([]string, 0, len(refs))
			for _, s := range refs {
				r, ok := parseRef(s)
				if !ok {
					return usageErr("--ref %q: an issue or pull request is owner/repo#n or its URL", s)
				}
				if name := refName(r); !slices.Contains(linked, name) {
					linked = append(linked, name)
				}
			}
			n := state.Note{For: forWho, Text: draft.text(), Due: d.UTC(), Default: draft.Default, By: me, At: a.now.UTC(), Until: draft.Until, Pinned: pin, Refs: slices.Clip(linked),
				Question: strings.TrimSpace(draft.Question), StatusQuo: draft.StatusQuo, Options: draft.Options, Recommend: draft.Recommend}
			person := a.cfg.Guide.Person
			switch {
			case draft.Kind == "":
				draft.Kind = noteKind(person, &n)
			case !slices.Contains(noteKinds, draft.Kind):
				return usageErr("--kind %q is none of %s", draft.Kind, strings.Join(noteKinds, ", "))
			case draft.Kind == noteDecision && forWho == "":
				return usageErr("a decision names who decides: --for <person>")
			}
			n.Kind, draft.Due = draft.Kind, due
			checked := forWho != "" && n.Kind != noteMemo && guides(person, &n)
			if checked {
				m := draft.missing()
				stages, err := planStages(cmd.Context(), a.cfg.Plans, draft.text())
				if err != nil {
					return err
				}
				if m = append(m, stages...); len(m) > 0 {
					return usageErr("note for %s refused, it lacks: %s", forWho, strings.Join(m, "; "))
				}
			} else if m := draft.unanswered(); n.Kind == noteDecision && len(m) > 0 {
				return usageErr("decision for %s refused, it lacks: %s", forWho, strings.Join(m, "; "))
			}
			if n.Kind == noteDecision {
				if m := draft.unrenderable(forWho); len(m) > 0 {
					return usageErr("decision for %s refused, it cannot render: %s", forWho, strings.Join(m, "; "))
				}
			} else if draft.Recommend != 0 {
				return usageErr("--recommend is a decision's: a %s recommends nothing", n.Kind)
			}
			if forWho != "" && n.Kind != noteMemo {
				if err := a.warnAnswered(cmd, n, draft.Question); err != nil {
					return err
				}
			}
			old := 0
			if replaces != "" {
				ids, err := parseIDs([]string{replaces}, "note")
				if err != nil {
					return err
				}
				old = ids[0]
			}
			var folded *state.Note
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				var replaced *state.Note
				if old != 0 {
					i := slices.IndexFunc(st.Notes, func(o state.Note) bool { return o.ID == old })
					if i < 0 {
						return nil, refused("note #%d is not open: nothing to replace", old)
					}
					r := st.Notes[i]
					replaced = &r
					st.Notes = slices.Delete(st.Notes, i, i+1)
					n.Pinned = n.Pinned || r.Pinned
				} else if checked {
					if o, ref := foldTarget(st.Notes, n, draft.Question); o != nil {
						o.Text += fmt.Sprintf(" | Also from %s: %s", me.Name, n.Text)
						folded = o
						return []state.Event{event(me, "note.folded", "into #%d (%s, %s): %s", o.ID, ref, verb(draft.Question), n.Text)}, nil
					}
				}
				st.NextNote++
				n.ID = st.NextNote
				st.Notes = append(st.Notes, n)
				evs := []state.Event{event(me, "note.add", "#%d %s", n.ID, n.Text)}
				if replaced != nil {
					evs = append(evs, event(me, noteReplaced, "#%d replaced by #%d: %s", replaced.ID, n.ID, replaced.Text))
				}
				return evs, nil
			})
			if err != nil {
				return err
			}
			if folded != nil {
				_, err = fmt.Fprintf(a.out, "note #%d (folded: it asks the same on the same issue or PR)\n", folded.ID)
				return err
			}
			if old != 0 {
				_, err = fmt.Fprintf(a.out, "note #%d (replaces #%d)\n", n.ID, old)
				return err
			}
			_, err = fmt.Fprintf(a.out, "note #%d\n", n.ID)
			return err
		},
	}
	add.Flags().StringVar(&forWho, "for", "", "who has to act: a person's name or email, or team:<name>")
	add.Flags().StringVar(&due, "due", "", "when it is due: a time (22:55) or a duration (3h)")
	add.Flags().StringVar(&draft.Default, "default", "", "what happens if nobody answers by the due time: an action")
	add.Flags().StringVar(&draft.StatusQuo, "status-quo", "", "what is true now")
	add.Flags().StringVar(&draft.Why, "why", "", "why it needs the person")
	add.Flags().StringArrayVar(&draft.Options, "option", nil, `a choice and its consequence, "<choice>: <consequence>" (repeatable)`)
	add.Flags().IntVar(&draft.Recommend, "recommend", 0, "the option recommended, 1-based (why goes into --status-quo)")
	add.Flags().StringVar(&draft.Checked, "checked", "", "where a state claim (merged, green, released, rolled, closed) was checked")
	add.Flags().BoolVar(&pin, "pin", false, "a standing instruction: every hand-over carries it until unpinned")
	add.Flags().StringVar(&draft.Kind, "kind", "", `"decision" (the default for the guide's person), "memo" (the default otherwise) or "login" (a sign-in, closed once --until passes)`)
	add.Flags().StringVar(&replaces, "replaces", "", "a note this one replaces: it closes in the same step (note.replaced)")
	add.Flags().StringVar(&draft.Until, "until", "", "a login note's probe: a shell command that exits 0 once signed in")
	add.Flags().StringArrayVar(&refs, "ref", nil, "an issue or pull request the note asks about, owner/repo#n (repeatable): the note closes once all are closed or merged")
	done := &cobra.Command{
		Use:   "done <id>...",
		Short: "Mark notes done",
		Long: `Mark notes done. --overtaken "<why>" closes them as overtaken
instead: what they ask about is settled without an answer, logged as
note.overtaken with the reason, as the watch does.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			ids, err := parseIDs(args, "note")
			if err != nil {
				return err
			}
			why := strings.TrimSpace(overtaken)
			if cmd.Flags().Changed("overtaken") && why == "" {
				return usageErr("--overtaken needs the reason")
			}
			var evs []state.Event
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Notes = slices.DeleteFunc(st.Notes, func(n state.Note) bool {
					if slices.Contains(ids, n.ID) {
						if why != "" {
							evs = append(evs, overtakenEvent(me, n, why))
						} else {
							evs = append(evs, event(me, noteDone, "#%d %s", n.ID, n.Text))
						}
						return true
					}
					return false
				})
				return evs, nil
			})
			if err != nil {
				return err
			}
			if len(evs) < len(ids) {
				return refused("%d of the %d notes were open", len(evs), len(ids))
			}
			return nil
		},
	}
	done.Flags().StringVar(&overtaken, "overtaken", "", "close them as overtaken: why what they ask is settled")
	var choice int
	var via string
	answer := &cobra.Command{
		Use:   "answer <id> [<answer>]",
		Short: "Record a person's answer on a note, word for word, and close it",
		Long: `Close the note with the person's answer: --choice <n>, one of its options
(1-based), the person's own words, or both. The note.answered event carries
the answer verbatim, with the way it came (--via, cli unless it came from
Slack), so the session that filed the note, the supervisor and the guide's
feed read it from the log (beekeeper log --verb note.answered); the session
that filed it is told at once.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			ids, err := parseIDs(args[:1], "note")
			if err != nil {
				return err
			}
			if via != viaCLI && via != viaSlack {
				return usageErr("--via %q is none of %s, %s", via, viaCLI, viaSlack)
			}
			var n state.Note
			var text string
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				i := slices.IndexFunc(st.Notes, func(n state.Note) bool { return n.ID == ids[0] })
				if i < 0 {
					return nil, refused("note #%d is not open", ids[0])
				}
				n = st.Notes[i]
				var err error
				if text, err = answerText(&n, choice, strings.Join(args[1:], " ")); err != nil {
					return nil, err
				}
				st.Notes = slices.Delete(st.Notes, i, i+1)
				return []state.Event{answeredEvent(me, &n, via, text)}, nil
			})
			if err != nil {
				return err
			}
			a.tellFiler(cmd.Context(), me, &n, answeredLine(n.ID, cmp.Or(me.Person, me.Name), text))
			_, err = fmt.Fprintf(a.out, "note #%d answered and closed\n", ids[0])
			return err
		},
	}
	answer.Flags().IntVar(&choice, "choice", 0, "the option chosen, 1-based")
	answer.Flags().StringVar(&via, "via", viaCLI, "how the answer came: cli or slack")
	var listFor string
	list := listCmd("List the open notes", func() error { return a.noteList(listFor) })
	list.Flags().StringVar(&listFor, "for", "", "only the notes for this person or team")
	c.AddCommand(add, answer, done, a.notePinCmd("pin", true), a.notePinCmd("unpin", false), list)
	return c
}

// noteList prints the open notes, those for forWho alone with it set.
func (a *app) noteList(forWho string) error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	notes := slices.Clone(st.Notes)
	if forWho != "" {
		notes = slices.DeleteFunc(notes, func(n state.Note) bool { return !strings.EqualFold(n.For, forWho) })
	}
	if a.json {
		for i := range notes {
			notes[i].Kind = noteKind(a.cfg.Guide.Person, &notes[i])
		}
		return a.printJSON(notes)
	}
	a.printNotes(notes)
	return nil
}

// printNotes lists notes, the decisions apart from the memos once both
// are among them.
func (a *app) printNotes(notes []state.Note) {
	if len(notes) == 0 {
		_, _ = fmt.Fprintln(a.out, "no open notes")
		return
	}
	var decisions, memos []state.Note
	for _, n := range notes {
		if decides(a.cfg.Guide.Person, &n) {
			decisions = append(decisions, n)
		} else {
			memos = append(memos, n)
		}
	}
	if len(decisions) == 0 || len(memos) == 0 {
		a.printNoteLines(notes)
		return
	}
	_, _ = fmt.Fprintf(a.out, "Decisions (%d):\n", len(decisions))
	a.printNoteLines(decisions)
	_, _ = fmt.Fprintf(a.out, "Memos (%d):\n", len(memos))
	a.printNoteLines(memos)
}

// printNoteLines prints one line per note.
func (a *app) printNoteLines(notes []state.Note) {
	for _, n := range notes {
		var tags []string
		if n.Pinned {
			tags = append(tags, "pinned")
		}
		if n.For != "" {
			tags = append(tags, "for "+n.For)
		}
		if n.By.Owner() != "" {
			tags = append(tags, "by "+quotedOwner(n.By))
		}
		if !n.Due.IsZero() {
			tags = append(tags, "due "+clock(a.now, n.Due))
			if n.Due.Before(a.now) {
				tags[len(tags)-1] += " (overdue)"
			}
		}
		tag := ""
		if len(tags) > 0 {
			tag = " [" + strings.Join(tags, ", ") + "]"
		}
		dflt := ""
		if n.Default != "" {
			dflt = " (default: " + n.Default + ")"
		}
		_, _ = fmt.Fprintf(a.out, "#%d%s %s%s\n", n.ID, tag, n.Text, dflt)
	}
}

// notePinCmd pins or unpins notes: a pinned note is in every hand-over.
func (a *app) notePinCmd(name string, pin bool) *cobra.Command {
	short := "Pin notes: every hand-over carries them until unpinned"
	if !pin {
		short = "Unpin notes: they are open notes again"
	}
	return &cobra.Command{
		Use:   name + " <id>...",
		Short: short,
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			ids, err := parseIDs(args, "note")
			if err != nil {
				return err
			}
			var evs []state.Event
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				for i := range st.Notes {
					if n := &st.Notes[i]; slices.Contains(ids, n.ID) {
						n.Pinned = pin
						evs = append(evs, event(me, "note."+name, "#%d %s", n.ID, n.Text))
					}
				}
				return evs, nil
			})
			if err != nil {
				return err
			}
			if len(evs) < len(ids) {
				return refused("%d of the %d notes were open", len(evs), len(ids))
			}
			return nil
		},
	}
}

// warnAnswered warns when n asks again what was answered for the same
// person within answeredWindow; the note is filed all the same.
func (a *app) warnAnswered(cmd *cobra.Command, n state.Note, question string) error {
	recent, err := a.answeredSince(a.now.Add(-answeredWindow))
	if err != nil {
		return err
	}
	if an, ref := repeats(recent, n, question); an != nil {
		_, err = fmt.Fprintf(cmd.ErrOrStderr(), "warning: note #%d asked %s on %s and was answered %s for %s: %q\n",
			an.ID, verb(an.Question), ref, a.stamp(an.At), an.For, oneLine(an.Answer))
	}
	return err
}

// parseIDs reads note or timer ids, with or without their #.
func parseIDs(args []string, kind string) ([]int, error) {
	ids := make([]int, 0, len(args))
	for _, s := range args {
		id, err := strconv.Atoi(strings.TrimPrefix(s, "#"))
		if err != nil {
			return nil, usageErr("%q is not a %s id", s, kind)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
