package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/post"
	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) noteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "note",
		Short: "Questions for a person and sign-ins: decisions and logins",
		Long: `A note asks a person. A decision waits on its person and carries its due
time and its default, what happens when nobody answers by then; a login
asks its person to sign in and closes once its probe passes. The guide
puts them to the person (guide queue, guide watch). beekeeper watch
reports a note once when it is due. Notes appear in every hand-over until
marked done. A session's state is its hand-over, rendered from live
state; deferred work is a board item, a priority a board rank and a hold
a blocked-by (board rank, board block), or on a person a decision that
sets its item's Status (note add --holds).

Without a subcommand, lists the open notes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.noteList(cmd.ErrOrStderr(), "", false) },
	}
	var forWho, due, overtaken string
	var foldInto int
	var foldArg string
	var holds bool
	var refs []string
	var draft noteDraft
	add := &cobra.Command{
		Use:   "add <text>",
		Short: "Add a note",
		Long: `Add a note for a person (--for). --kind is decision, the default, or
login. A decision is refused without --due and a --default that is an
action. A decision for the guide's person (guide.person; with it unset,
any --for) is refused, naming what it lacks, unless it asks something (a
question mark, an --option or a request verb opening it; a status line goes
to beekeeper log add) and carries what the person needs to answer without
asking back: --status-quo and --why, every
--option as "<choice>: <consequence>", a --default that is an action (not
"wait" or "none"), the full URL of every #N or owner/repo#N it names, and
--checked "<source>" for a claim that something is merged, green,
released, rolled or closed. A note that links a pull request of a plans
repository (plans.repositories) is admitted when that pull request is
merged, or open with its stage check (plans.check, default plan-stages)
green or red only for missing stage outputs ("<plan>: <stage> missing"):
the grill stage, a revision awaiting its passes or a plan sent back by a
SIMPLIFY or KILL verdict, whose output is questions for the person. An
added note logs plan.grilling for each plan pull request it links at its
grill stage and plan.ready for each open one whose check is green, which
the event sink (sink.verbs) dispatches. It is
refused, naming the stage and what would admit it, while the pull request
is closed, the check is pending or missing on the head, or red for
anything else (the PRD fails its own check). A checked note folds into
the open note for the same person (note.folded) only when it asks the same
question (the same text once case, spacing and closing punctuation are set
aside) on the same issue or PR: its --ref, else the first URL of its
--status-quo; URLs cited as context and a shared first word never fold.
The same question with a different --default or recommended option is
refused, naming the open note: fold it with --fold-into <n> (the open
note's default and recommendation stand) or ask a different question.
--fold-into <n> folds into open note n for the same person whatever the
question. A --kind login note closes
once its --until probe, a shell command the watch runs every tick, exits 0.
A decision is refused without --ref: its answer is
recorded on that thread (beekeeper answer). A note for a person whose
--ref an answer of that person was recorded on within the last 72 hours
(answer.recorded) is filed with a warning that quotes the answer.

--holds holds every --ref of the decision on the board: each one's Status is
set to Blocked / Waiting, which board next offers in no step and names the
decision for as it skips the item, and the Status it had is recorded on the
note; one refused leaves none held. When the decision closes, answered
(beekeeper answer, note answer, the serve tool), by its default, as
overtaken or by hand, each item is set back to that Status (board.released, BOARD RELEASED)
and its filer told. An item whose Status was changed while held is left as
it is (HOLD LEFT), the filer told why; one another open decision holds stays
held until that one closes (HOLD KEPT). --holds without --ref, on an issue
that is not on the board or has no Status, on a sign-in or with --fold-into
is refused.

--ref links the note to an issue or pull request it asks about
(owner/repo#n or its URL, repeatable; the first is the thread its answer
is recorded on): once every linked one is closed or
merged, beekeeper watch closes the note as overtaken (note.overtaken),
when its worker closed them at the end of its task: an issue a pull
request's closing keyword closed at its merge, or any close while the
worker that filed the note still runs its task, keeps the note open
(NOTE KEPT, note.kept, once per reason); once the worker reports its task
over (agents idle), its settled issues overtake its notes.
A note without --ref stays open: once the session that filed it is
archived, guide watch names it to the guide as orphaned (GUIDE ORPHANED),
to ask or close by hand; a role's run never orphans its notes.

A decision is put to its addressee as one message (beekeeper serve, through
klaus-gateway): --for a person, or --for team:<name> for any member of the
team. It is refused unless it renders: the question one line of at most 150
characters, --status-quo at most 3000, at most 10 --option with labels of at
most 75 characters, --recommend <n> naming one of them. At its due time
beekeeper watch closes it with its default (note.defaulted, NOTE DEFAULTED)
and tells the session that filed it.

On a machine with a central instance (central.context), a decision is filed
there (note_add, through muster as the person) once its checks pass: the
central instance puts it in Slack and its answer or default reaches the
filing session through the person's mailbox. Its number is the central
note's. A sign-in, --holds and --fold-into a machine's note stay on the
machine; --fold-into <context>#<n>, or a bare <n> only the central instance
has open, folds a decision into the central instance's note. With the
central instance unreachable before the call went out, the decision is
filed on the machine, as stderr says; one sent and left unanswered is
filed nowhere else and exits 69.

A decision from a worker on the roster (or on another host) is classified
before any other check. An operational ask, which the supervisor loop or the
desk's machinery owns, is refused naming where it goes: a review (the loop
starts the desk review on agents park --on <pr>), CI, runners or builds,
disk, capacity or rate limits, rolling a merged change out to a checkout or
machine, and starting or stopping sessions. A note on a plans repository's
pull request is the person's plan review, and a question of spending, cost,
consent, accounts or credentials, or anything public or in production the
person's own decision: neither is one. The classification runs before
--fold-into and any fold, local or central. A decision whose --ref
is closed or merged, or whose issue's parent epic is closed, is refused as
superseded; a thread GitHub does not answer is filed with a warning. Each
refusal logs note.redirected; only what is the person's to decide (live,
public, costly or reserved for them) reaches them.`,
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
			if a.managedExecutionID() != "" && (foldArg != "" || holds || draft.Kind == noteLogin) {
				return refused("remote notes support ordinary decisions only; use the coordinating component for holds, logins and folds")
			}
			var into noteID
			if foldArg != "" {
				if into, err = a.noteNamed(foldArg); err != nil {
					return err
				}
				foldInto = into.ID
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
			draft.Here = post.SingleRepo(strings.Join(linked, " "))
			n := state.Note{For: forWho, Text: draft.text(), Due: d.UTC(), Default: draft.Default, By: me, At: a.now.UTC(), Until: draft.Until, Refs: slices.Clip(linked),
				Question: strings.TrimSpace(draft.Question), StatusQuo: draft.StatusQuo, Options: draft.Options, Recommend: draft.Recommend}
			person := a.cfg.Guide.Person
			draft.Kind = cmp.Or(draft.Kind, noteDecision)
			switch {
			case !slices.Contains(noteKinds, draft.Kind):
				return usageErr("--kind %q is none of %s", draft.Kind, strings.Join(noteKinds, ", "))
			case forWho == "":
				return usageErr("a note asks a person: --for <person>; a session's state is its hand-over, deferred work a board item")
			case draft.Kind == noteLogin && strings.TrimSpace(draft.Until) == "":
				return usageErr(`a login closes on its probe: --until "<command that exits 0 once signed in>"`)
			case draft.Kind != noteLogin && draft.Until != "":
				return usageErr("--until is a login's: --kind login")
			}
			n.Kind, draft.Due = draft.Kind, due
			if holds {
				switch {
				case n.Kind != noteDecision:
					return refused("--holds is a decision's: a %s holds no board item", n.Kind)
				case len(linked) == 0:
					return refused("--holds needs --ref: the board items it holds are its --ref")
				case foldInto != 0:
					return refused("--holds with --fold-into: the open note's hold stands; file the hold on a note of its own")
				}
			}
			// Classified before any fold: a fold puts the ask before the
			// open note's person as surely as a note of its own.
			if n.Kind == noteDecision {
				worker, err := a.isWorker(me)
				if err != nil {
					return err
				}
				if worker {
					if err := a.routeWorkerNote(cmd.Context(), cmd.ErrOrStderr(), me, draft, linked); err != nil {
						return err
					}
				}
			}
			checked := guides(person, &n)
			var stage planStage
			if checked {
				m := draft.missing()
				stage, err = planStages(cmd.Context(), a.cfg.Plans, draft.subject())
				if err != nil {
					return err
				}
				if m = append(m, stage.Refused...); len(m) > 0 {
					return usageErr("note for %s refused, it lacks: %s", forWho, strings.Join(m, "; "))
				}
				for _, g := range stage.Grill {
					if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "admitted: %s; the note carries its questions for %s\n", g, forWho); err != nil {
						return err
					}
				}
			} else if m := draft.unanswered(); n.Kind == noteDecision && len(m) > 0 {
				return usageErr("decision for %s refused, it lacks: %s", forWho, strings.Join(m, "; "))
			}
			if n.Kind == noteDecision {
				if m := draft.unrenderable(forWho); len(m) > 0 {
					return usageErr("decision for %s refused, it cannot render: %s", forWho, strings.Join(m, "; "))
				}
				if len(linked) == 0 {
					return usageErr("decision for %s refused, it names no thread: --ref owner/repo#n, the issue or pull request it concerns, where its answer is recorded", forWho)
				}
			} else if draft.Recommend != 0 {
				return usageErr("--recommend is a decision's: a %s recommends nothing", n.Kind)
			}
			if a.managedExecutionID() != "" {
				return a.remoteControl(cmd.Context(), "note", executionNote{Draft: draft, Note: n})
			}
			if err := a.warnAnswered(cmd, n); err != nil {
				return err
			}
			if into.Central {
				if n.Kind != noteDecision {
					return refused("a %s folds into the machine's notes only: %s#%d is the central instance's", n.Kind, a.cfg.Central.Context, foldInto)
				}
				return a.foldCentral(cmd.Context(), me, &n, draft, foldInto)
			}
			if a.centralNote(&n, holds, foldInto) {
				if filed, err := a.fileCentral(cmd.Context(), cmd.ErrOrStderr(), me, &n, draft, stage); filed || err != nil {
					return err
				}
			}
			if holds {
				st, err := a.store.Read()
				if err != nil {
					return err
				}
				if err = a.holdAll(cmd.Context(), st.Notes, &n, linked); err != nil {
					return err
				}
			}
			var folded *state.Note
			fold := func(o *state.Note, why string) []state.Event {
				o.Text += fmt.Sprintf(" | Also from %s: %s", me.Name, n.Text)
				if o.Held == nil {
					o.Held, o.AlsoHeld = n.Held, n.AlsoHeld
				}
				folded = o
				return []state.Event{event(me, "note.folded", "into #%d (%s): %s", o.ID, why, n.Text)}
			}
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				if foldInto != 0 {
					i := slices.IndexFunc(st.Notes, func(o state.Note) bool { return o.ID == foldInto })
					if i < 0 {
						return nil, refused("note #%d is not open: nothing to fold into", foldInto)
					}
					if o := &st.Notes[i]; !strings.EqualFold(o.For, n.For) {
						return nil, refused("note #%d is for %q, this one for %q: a note folds into one for the same person", o.ID, o.For, n.For)
					}
					return fold(&st.Notes[i], "--fold-into"), nil
				} else if checked {
					if o, ref := foldTarget(st.Notes, &n); o != nil {
						if c := foldConflict(o, &n); len(c) > 0 {
							return nil, refused("note #%d for %s asks the same question on %s, but %s: fold into it with --fold-into %d (its default and recommendation stand) or ask a different question",
								o.ID, o.For, ref, strings.Join(c, " and "), o.ID)
						}
						return fold(o, ref+", the same question"), nil
					}
				}
				st.NextNote++
				n.ID = st.NextNote
				st.Notes = append(st.Notes, n)
				return append([]state.Event{event(me, "note.add", "#%d %s", n.ID, n.Text)}, stage.events(me, n.ID, forWho)...), nil
			})
			if err != nil {
				return err
			}
			if folded != nil {
				why := "it asks the same question on the same issue or PR"
				if foldInto != 0 {
					why = "--fold-into: its default and recommendation stand"
				}
				_, err = fmt.Fprintf(a.out, "note #%d (folded: %s)\n", folded.ID, why)
				return err
			}
			if _, err = fmt.Fprintf(a.out, "note #%d\n", n.ID); err != nil {
				return err
			}
			for _, h := range n.Holds() {
				if _, err = fmt.Fprintf(a.out, "holds %s: %s until the decision closes, then %s again\n", h.Ref, h.Status, h.Was); err != nil {
					return err
				}
			}
			return nil
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
	add.Flags().StringVar(&draft.Kind, "kind", "", `"decision" (the default) or "login" (a sign-in, closed once --until passes)`)
	add.Flags().StringVar(&draft.Until, "until", "", "a login note's probe: a shell command that exits 0 once signed in")
	add.Flags().StringVar(&foldArg, "fold-into", "", "an open note for the same person this one folds into, whatever it asks (<n>, or <context>#<n> for a central instance's): that note's default and recommendation stand")
	add.Flags().BoolVar(&holds, "holds", false, "the decision holds every --ref: its board Status is Blocked / Waiting until the decision closes, then set back")
	add.Flags().StringArrayVar(&refs, "ref", nil, "an issue or pull request the note asks about, owner/repo#n (repeatable): the note closes once all are closed or merged")
	done := &cobra.Command{
		Use:   "done <id>...",
		Short: "Mark notes done",
		Long: `Mark notes done. --overtaken "<why>" closes them as overtaken
instead: what they ask about is settled without an answer, logged as
note.overtaken with the reason, as the watch does.

On a machine with a central instance (central.context), <context>#<n>
withdraws the central instance's note n (note_done, through muster as the
person): its filer's, or the filer's team's supervisor role's. A bare <n>
is the machine's note when it has one open, else the central instance's;
one open on both is refused, naming both. --overtaken is the machine's
notes' only.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			named, err := a.parseNoteIDs(args)
			if err != nil {
				return err
			}
			why := strings.TrimSpace(overtaken)
			if cmd.Flags().Changed("overtaken") && why == "" {
				return usageErr("--overtaken needs the reason")
			}
			ids, centralIDs, err := a.doneSources(named)
			if err != nil {
				return err
			}
			if len(centralIDs) > 0 && why != "" {
				return usageErr("--overtaken is for this machine's notes: a central note is withdrawn (note done %s#%d) without a reason", a.cfg.Central.Context, centralIDs[0])
			}
			if len(ids) == 0 {
				return a.doneCentral(cmd.Context(), me, centralIDs)
			}
			var evs []state.Event
			var closed []state.Note
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				evs, closed = nil, nil
				// Two teams' notes of one number (numbered before the
				// shared note counter): the caller's team's is the one meant.
				teams := map[int]int{}
				for _, n := range st.Notes {
					teams[n.ID]++
				}
				st.Notes = slices.DeleteFunc(st.Notes, func(n state.Note) bool {
					if slices.Contains(ids, n.ID) && (teams[n.ID] == 1 || n.By.Team == me.Team) {
						closed = append(closed, n)
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
			how := closedDone
			if why != "" {
				how = closedOvertaken
			}
			for _, l := range a.releaseHolds(cmd.Context(), me, closed, how) {
				if _, err := fmt.Fprintln(a.out, l); err != nil {
					return err
				}
			}
			if err := a.doneCentral(cmd.Context(), me, centralIDs); err != nil {
				return err
			}
			if len(evs) < len(ids) {
				return refused("%d of the %d notes were open", len(evs)+len(centralIDs), len(ids)+len(centralIDs))
			}
			return nil
		},
	}
	done.Flags().StringVar(&overtaken, "overtaken", "", "close them as overtaken: why what they ask is settled")
	var choice int
	var via string
	var post bool
	answer := &cobra.Command{
		Use:   "answer <id> [<answer>]",
		Short: "Record a person's answer to a note, word for word, and close it",
		Long: `beekeeper answer for a note: its thread is the note's first --ref. The
answer is --choice <n>, one of its options (1-based), the person's own
words (at most 3000 characters), or both, with the way it came (--via, cli unless it came from
Slack). It is logged as answer.recorded and the note closed; the session
that filed it is told at once. A decision its default closed within the
last day is answered all the same: the answer replaces the default.
Nothing is posted to GitHub unless --post (as beekeeper answer says). A note a role's run filed (the
supervisor's, the guide's) goes to the role's holder now; while none holds
it, the answer waits for the role's next start. A note without a --ref is
refused: beekeeper answer <owner/repo#n> --note <id> names its thread.

On a machine with a central instance (central.context), <context>#<n>
answers the central instance's note n (note_answer, through muster as the
person): only its addressee answers it, and the central instance closes
its Slack message and tells its filer. A bare <n> is the machine's note
when it has it, else the central instance's; one open on both is refused,
naming both. --post is the machine's notes' only.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ids, err := a.parseNoteIDs(args[:1])
			if err != nil {
				return err
			}
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			n, _ := answerable(st, ids[0].ID, a.now)
			if ids[0].Central {
				n = nil
			} else if a.centralNotesOn() && n != nil {
				// The same number open centrally too is ambiguous; one
				// the machine lacks is answered there, open or not.
				central, cerr := a.listCentralNotes()
				if ids[0], err = a.resolve(ids[0], true, central, cerr); err != nil {
					return err
				}
			} else if a.centralNotesOn() {
				ids[0].Central = true
			}
			if ids[0].Central {
				return a.answerCentral(cmd.Context(), ids[0].ID, choice, via, strings.Join(args[1:], " "), post)
			}
			if n == nil {
				return refused("note #%d is not open, nor closed by its default within %s", ids[0].ID, shortDur(defaultedKeep))
			}
			if len(n.Refs) == 0 {
				return refused("note #%d links no issue or pull request (--ref) for its answer's thread: beekeeper answer <owner/repo#n> --note %d", n.ID, n.ID)
			}
			return a.recordAnswer(cmd.Context(), n.Refs[0], n.ID, choice, via, strings.Join(args[1:], " "), post)
		},
	}
	answer.Flags().IntVar(&choice, "choice", 0, "the option chosen, 1-based")
	answer.Flags().StringVar(&via, "via", viaCLI, "how the answer came: cli or slack")
	answer.Flags().BoolVar(&post, "post", false, "also record the decision as a GitHub comment")
	var listFor string
	var full bool
	list := &cobra.Command{Use: "list", Short: "List the open notes", Args: cobra.NoArgs}
	list.RunE = func(cmd *cobra.Command, _ []string) error { return a.noteList(cmd.ErrOrStderr(), listFor, full) }
	list.Long = `List the open notes, one line each. --full prints each note's whole
text, every line of it, followed by its default and the issues and pull
requests it names (owner/repo#n, from its text and its --ref links; a bare
#n after the board owner's repository), so a scan sees what a note names
without reading each one; with --json, --full adds each note's names.

On a machine with a central instance (central.context), the central
instance's open notes (note_list, through muster as the person) follow the
machine's, each named <context>#<n> (in JSON its "central"); with the
central instance unreachable, the list fails after the machine's notes.`
	list.Flags().StringVar(&listFor, "for", "", "only the notes for this person or team")
	list.Flags().BoolVar(&full, "full", false, "each note's whole text and the issues and pull requests it names")
	c.AddCommand(add, answer, done, list)
	return c
}

// noteList prints the open notes, those for forWho alone with it set;
// full prints each note's whole text and what it names.
func (a *app) noteList(stderr io.Writer, forWho string, full bool) error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	notes := slices.Clone(st.Notes)
	// The central instance's notes follow the machine's; unreachable, the
	// list fails after the machine's.
	central, cerr := a.listCentralNotes()
	if cerr != nil {
		_, _ = fmt.Fprintf(stderr, "warning: %v\n", cerr)
	}
	notes = append(notes, central...)
	if forWho != "" {
		notes = slices.DeleteFunc(notes, func(n state.Note) bool { return !strings.EqualFold(n.For, forWho) })
	}
	if a.json {
		if !full {
			return errors.Join(a.printJSON(notes), cerr)
		}
		named := make([]map[string]any, 0, len(notes))
		for _, n := range notes {
			m, err := a.namedNote(&n)
			if err != nil {
				return err
			}
			named = append(named, m)
		}
		return errors.Join(a.printJSON(named), cerr)
	}
	if full {
		a.printNotesWith(notes, a.printNotesFull)
	} else {
		a.printNotes(notes)
	}
	return cerr
}

// listCentralNotes are the central instance's open notes as the caller
// reads them, its error naming the instance.
func (a *app) listCentralNotes() ([]state.Note, error) {
	if !a.centralNotesOn() {
		return nil, nil
	}
	me, _ := a.caller()
	notes, err := a.centralNotes(context.Background(), me)
	if err != nil {
		return nil, lookupErr(err, "the central instance's notes (%s) are not listed", a.cfg.Central.Context)
	}
	return notes, nil
}

// doneSources splits the notes note done names into the machine's and the
// central instance's (resolve).
func (a *app) doneSources(named []noteID) (local, central []int, err error) {
	var open []state.Note
	var cerr error
	if a.centralNotesOn() && slices.ContainsFunc(named, func(id noteID) bool { return id.Bare }) {
		open, cerr = a.listCentralNotes()
	}
	st, err := a.store.Read()
	if err != nil {
		return nil, nil, err
	}
	for _, id := range named {
		has := slices.ContainsFunc(st.Notes, func(n state.Note) bool { return n.ID == id.ID })
		if id, err = a.resolve(id, has, open, cerr); err != nil {
			return nil, nil, err
		}
		if id.Central {
			central = append(central, id.ID)
		} else {
			local = append(local, id.ID)
		}
	}
	return local, central, nil
}

// namedNote is n's JSON object with the issues and pull requests it names
// as its "names"; a note's own encoding keeps the fields it does not know.
func (a *app) namedNote(n *state.Note) (map[string]any, error) {
	b, err := json.Marshal(n)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if names := a.noteNames(n); len(names) > 0 {
		m["names"] = names
	}
	return m, nil
}

// noteNames are the issues and pull requests (owner/repo#n, lower-cased)
// n names in its text or links with --ref, as a board scan reads them.
func (a *app) noteNames(n *state.Note) []string {
	return namedRefs(strings.Join(append([]string{n.Text}, n.Refs...), " "), a.cfg.Board.Owner)
}

// printNotes lists notes one line each.
func (a *app) printNotes(notes []state.Note) { a.printNotesWith(notes, a.printNoteLines) }

// printNotesWith lists notes with print.
func (a *app) printNotesWith(notes []state.Note, print func([]state.Note)) {
	if len(notes) == 0 {
		_, _ = fmt.Fprintln(a.out, "no open notes")
		return
	}
	print(notes)
}

// printNoteLines prints one line per note.
func (a *app) printNoteLines(notes []state.Note) {
	for _, n := range notes {
		dflt := ""
		if n.Default != "" {
			dflt = " (default: " + n.Default + ")"
		}
		_, _ = fmt.Fprintf(a.out, "%s%s %s%s\n", noteRef(&n), a.noteTag(&n), n.Text, dflt)
	}
}

// printNotesFull prints each note's id and tags, then its whole text, its
// default and what it names, indented beneath.
func (a *app) printNotesFull(notes []state.Note) {
	for _, n := range notes {
		_, _ = fmt.Fprintf(a.out, "%s%s\n", noteRef(&n), a.noteTag(&n))
		for line := range strings.Lines(strings.TrimRight(n.Text, "\n")) {
			_, _ = fmt.Fprintf(a.out, "  %s\n", strings.TrimRight(line, "\n"))
		}
		if n.Default != "" {
			_, _ = fmt.Fprintf(a.out, "  default: %s\n", n.Default)
		}
		if names := a.noteNames(&n); len(names) > 0 {
			_, _ = fmt.Fprintf(a.out, "  names: %s\n", strings.Join(names, ", "))
		}
	}
}

// noteTag is n's tags, " [for …, by …, due …]", or "".
func (a *app) noteTag(n *state.Note) string {
	var tags []string
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
	if len(tags) == 0 {
		return ""
	}
	return " [" + strings.Join(tags, ", ") + "]"
}

// warnAnswered warns when the person n is for answered on one of its
// threads within answeredWindow; the note is filed all the same.
func (a *app) warnAnswered(cmd *cobra.Command, n state.Note) error {
	recent, err := a.answeredSince(a.now.Add(-answeredWindow))
	if err != nil {
		return err
	}
	if an := repeats(recent, n); an != nil {
		at := ""
		if an.Comment != "" {
			at = " (" + an.Comment + ")"
		}
		_, err = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %q answered on %s %s%s: %q\n",
			an.For, an.About, a.stamp(an.At), at, oneLine(an.Answer))
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
