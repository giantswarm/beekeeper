package cmd

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/post"
	"github.com/giantswarm/beekeeper/internal/state"
)

// blockedStatus is the board's Status of a blocked item, an unambiguous
// part of its name ("Blocked / Waiting ⛔️").
const blockedStatus = "blocked"

// escalateWhat opens the text of an escalation's re-check timer; the rest
// is the item's ref. An open timer with it means the item is escalated.
const escalateWhat = "re-check blocked "

// escalateComments is how many of an item's newest comments board
// escalate reads.
const escalateComments = 5

// askedFlag carries, on a re-check timer's run, the state of the item its
// note was filed on (escalation.State).
const askedFlag = "asked"

// escalation is one blocked item to escalate to the person.
type escalation struct {
	Ref   string `json:"ref"`
	URL   string `json:"url"`
	Title string `json:"title"`
	// Why is what holds it: its open recorded blockers, or its Status.
	Why string `json:"why"`
	// Since is when its Status was set; zero when the board does not say.
	Since time.Time `json:"since,omitzero"`
	// Who is who it waits on; Evidence the facts read for it, one each.
	Who      string   `json:"who,omitempty"`
	Evidence []string `json:"evidence,omitempty"`
	// State fingerprints what holds it: unchanged, it is not asked again.
	State string `json:"state,omitempty"`
	// Skip is why no note is filed on it, Unchanged that its note was filed
	// on the same State.
	Skip      string `json:"skip,omitempty"`
	Unchanged bool   `json:"unchanged,omitempty"`
	// Asked is the State the last note on it was filed on, "" for none: a
	// skip's re-check keeps it, so a skip never makes an unchanged item new.
	Asked string `json:"asked,omitempty"`

	item board.Item
}

// escalations are the open board items in the blocked Status, refs the
// ones named (all when empty), that have no open re-check timer: each is
// looked at once, and again when its timer fires while it stays blocked.
// An item whose recorded blockers all closed is left out when the order's
// blocker-cleared step offers it as work: its Status was set before the
// last of them closed. One whose Status was set after is held by it
// (HeldByStatus), offered by no step, and escalated.
func escalations(items []board.Item, blocked string, timers []state.Timer, refs []string) []escalation {
	var out []escalation
	for _, it := range items {
		if !strings.EqualFold(it.Status, blocked) || (it.Blockers > 0 && it.OpenBlockers == 0 && !it.HeldByStatus()) {
			continue
		}
		if len(refs) > 0 && !slices.ContainsFunc(refs, func(r string) bool { return strings.EqualFold(r, it.Ref) }) {
			continue
		}
		if slices.ContainsFunc(timers, func(t state.Timer) bool { return strings.EqualFold(t.What, escalateWhat+it.Ref) }) {
			continue
		}
		why := "Status " + it.Status
		if it.OpenBlockers > 0 {
			why = it.HeldText()
		}
		out = append(out, escalation{Ref: it.Ref, URL: it.URL, Title: it.Title, Why: why, Since: it.StatusSet, item: it})
	}
	return out
}

// escalateFacts is what board escalate reads about one blocked item beside
// the board: its newest comments, the workers parked on it and the
// person's open notes and recorded answers on its thread.
type escalateFacts struct {
	// People are board.people, Person guide.person.
	People []string
	Person string
	// Comments are its newest comments, oldest first; CommentsErr why they
	// could not be read.
	Comments    []github.Comment
	CommentsErr error
	Parked      []state.Agent
	Notes       []state.Note
	Answers     []answered
	// Asked is the State its last note was filed on (askedFlag).
	Asked string
}

// lastPersonComment is the newest comment a person wrote, nil for none.
func lastPersonComment(cs []github.Comment) *github.Comment {
	for i := len(cs) - 1; i >= 0; i-- {
		if !cs[i].Automation {
			return &cs[i]
		}
	}
	return nil
}

// stateOf fingerprints what holds it: its Status and when it was set, its
// open blockers, its assignees and its newest comment by a person. An
// automation's comment (a desk's claim) changes nothing the person decides.
func stateOf(it board.Item, last *github.Comment) string {
	lower := func(s []string) []string {
		out := make([]string, 0, len(s))
		for _, x := range s {
			out = append(out, strings.ToLower(x))
		}
		slices.Sort(out)
		return out
	}
	parts := []string{it.Status, it.StatusSet.UTC().Format(time.RFC3339),
		strings.Join(lower(it.OpenBlockedBy), ","), strings.Join(lower(it.Assignees), ",")}
	if last != nil {
		parts = append(parts, last.URL)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:6])
}

// agentsParkedOn are the agents parked on ref (owner/repo#n or its URL),
// and the parked agents whose task is ref: their sessions serve it, or their
// task names its URL. A worker parked on its pull request, a hold or the
// GitHub gate waits on its own item.
func agentsParkedOn(st *state.State, ref, url string) []state.Agent {
	var out []state.Agent
	for _, ag := range st.Agents {
		if ag.Park == nil {
			continue
		}
		if strings.EqualFold(ag.Park.On, ref) || strings.EqualFold(ag.Park.On, url) ||
			strings.EqualFold(LoopServes(st, ag), ref) || namesURL(ag.Task, url) {
			out = append(out, ag)
		}
	}
	return out
}

// namesURL reports whether text names url whole: …/issues/2 is not named by
// …/issues/21.
func namesURL(text, url string) bool {
	if url == "" {
		return false
	}
	lower, u := strings.ToLower(text), strings.ToLower(url)
	for i := 0; ; {
		j := strings.Index(lower[i:], u)
		if j < 0 {
			return false
		}
		end := i + j + len(u)
		if end == len(lower) || lower[end] < '0' || lower[end] > '9' {
			return true
		}
		i = end
	}
}

// explainSlack is how long before an item's Status was set a comment still
// explains it: a worker writes what holds the item, then moves it.
const explainSlack = 24 * time.Hour

// threadNamed is an issue or pull request a comment names: its URL or
// owner/repo#n.
var threadNamed = regexp.MustCompile(`(?i)https?://github\.com/([\w.-]+/[\w.-]+)/(?:issues|pull)/(\d+)|(?:^|[^\w/.#-])([\w.-]+/[\w.-]+)#(\d+)\b`)

// explaining is the newest of cs, written since its item's Status was set
// (less explainSlack; any when unknown), that names an issue or pull request
// other than ref, the item itself: the comment says what holds it. It
// returns the comment and the thread it names, nil for none.
func explaining(cs []github.Comment, ref string, since time.Time) (*github.Comment, string) {
	for i := len(cs) - 1; i >= 0; i-- {
		c := &cs[i]
		if !since.IsZero() && c.At.Before(since.Add(-explainSlack)) {
			break
		}
		for _, m := range threadNamed.FindAllStringSubmatch(c.Body, -1) {
			named := m[1] + "#" + m[2]
			if m[1] == "" {
				named = m[3] + "#" + m[4]
			}
			if !strings.EqualFold(named, ref) {
				return c, strings.ToLower(named)
			}
		}
	}
	return nil, ""
}

// notesOn are person's open notes on ref: linked to it (--ref) or holding
// it (--holds).
func notesOn(notes []state.Note, person, ref string) []state.Note {
	var out []state.Note
	for _, n := range notes {
		if !strings.EqualFold(n.For, person) {
			continue
		}
		if slices.ContainsFunc(n.Refs, func(r string) bool { return strings.EqualFold(r, ref) }) || n.HoldOf(ref) != nil {
			out = append(out, n)
		}
	}
	return out
}

// judge reads f into e: who and what it waits on with the evidence, its
// State, and whether it is asked about. It is not when a teammate is
// assigned to it, the person's queue holds a note on it, the person
// answered on its thread since it was blocked, the newest comment by a
// person comes from outside the organisation (a customer's), something
// explains the block (an open recorded blocker, a worker parked on it or on
// its task, a comment since it was blocked naming another issue or pull
// request), or its note was filed on the same State.
func (a *app) judge(e escalation, f escalateFacts) escalation {
	it := e.item
	linked := func(s string) string {
		here, _, _ := strings.Cut(e.Ref, "#")
		return post.Linked(s, nil, here)
	}
	last := lastPersonComment(f.Comments)
	e.State = stateOf(it, last)
	e.Asked = f.Asked

	// What it waits on and since when.
	since := "since an unknown time: the board records no change of its Status"
	if !it.StatusSet.IsZero() {
		since = fmt.Sprintf("since %s (%s ago)", it.StatusSet.UTC().Format(time.DateOnly), ago(a.now, it.StatusSet))
	}
	waits := "no open blocker is recorded (board block); only its Status holds it"
	if it.OpenBlockers > 0 {
		waits = linked(it.HeldText())
	}
	e.Evidence = append(e.Evidence, "in "+it.Status+" "+since, "waits on: "+waits)

	// Who.
	var who []string
	others := slices.DeleteFunc(slices.Clone(it.Assignees), func(l string) bool { return board.ContainsFold(f.People, l) })
	switch {
	case len(it.Assignees) == 0:
		e.Evidence = append(e.Evidence, "nobody is assigned")
	default:
		e.Evidence = append(e.Evidence, "assigned to "+strings.Join(it.Assignees, ", "))
		who = append(who, it.Assignees...)
	}
	for _, ag := range f.Parked {
		e.Evidence = append(e.Evidence, fmt.Sprintf("worker %s parked on it since %s: %s", ag.Name, ag.Park.At.UTC().Format(time.DateOnly), oneLine(ag.Park.Waits)))
		who = append(who, "worker "+ag.Name)
	}
	switch {
	case f.CommentsErr != nil:
		e.Evidence = append(e.Evidence, "its comments could not be read: "+oneLine(f.CommentsErr.Error()))
	case last == nil:
		e.Evidence = append(e.Evidence, fmt.Sprintf("no comment by a person among its newest %d", escalateComments))
	default:
		e.Evidence = append(e.Evidence, fmt.Sprintf("newest comment by a person: %s on %s, %s", last.Login, last.At.UTC().Format(time.DateOnly), last.URL))
		if !last.Member && !board.ContainsFold(f.People, last.Login) {
			who = append(who, fmt.Sprintf("%s (outside the organisation, GitHub's association %s)", last.Login, last.Association))
		}
	}
	e.Who = strings.Join(who, ", ")
	if e.Who == "" {
		e.Who = "nobody named: no assignee, no parked worker"
	}

	explainer, named := explaining(f.Comments, e.Ref, it.StatusSet)

	// Whether to ask.
	answeredOn := func() *answered {
		for i := len(f.Answers) - 1; i >= 0; i-- {
			an := &f.Answers[i]
			if strings.EqualFold(an.About, e.Ref) && strings.EqualFold(an.For, f.Person) && !an.At.Before(it.StatusSet) {
				return an
			}
		}
		return nil
	}
	switch notes := notesOn(f.Notes, f.Person, e.Ref); {
	case len(others) > 0:
		e.Skip = "assigned to " + strings.Join(others, ", ") + ", outside board.people: the wait is the assignee's"
	case len(notes) > 0:
		e.Skip = fmt.Sprintf("note #%d for %s on it is unanswered", notes[0].ID, f.Person)
	case answeredOn() != nil:
		an := answeredOn()
		e.Skip = fmt.Sprintf("%s answered on its thread %s: %q", an.For, an.At.UTC().Format(time.DateOnly), truncate(oneLine(an.Answer), 160))
	case last != nil && !last.Member && !board.ContainsFold(f.People, last.Login):
		e.Skip = fmt.Sprintf("its newest comment by a person is %s's, outside the organisation (GitHub's association %s; a teammate's login belongs in board.people) (%s): the wait is on them", last.Login, last.Association, last.URL)
	case it.OpenBlockers > 0:
		e.Skip = linked(it.HeldText()) + ": its recorded blockers explain it, board next offers it once they close"
	case len(f.Parked) > 0:
		ag := f.Parked[0]
		e.Skip = fmt.Sprintf("worker %s is parked on it (%s): the wait is the worker's", ag.Name, cmp.Or(ag.Park.On, oneLine(ag.Park.Waits)))
	case explainer != nil:
		e.Skip = fmt.Sprintf("its comment %s (%s, %s) names what holds it, %s", explainer.URL, explainer.Login, explainer.At.UTC().Format(time.DateOnly), linked(named))
	case f.Asked != "" && f.Asked == e.State:
		e.Unchanged = true
	}
	return e
}

// shellQuote quotes s as one word of a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// escalateRun is the re-check timer's command: this binary, its
// configuration and the caller, running board escalate on ref, with the
// State its note was filed on when asked is set.
func (a *app) escalateRun(me state.Party, ref, asked string) (string, error) {
	bin, err := os.Executable()
	if err != nil {
		return "", err
	}
	w := []string{shellQuote(bin)}
	if a.cfgPath != "" {
		w = append(w, "--"+configFlag, shellQuote(a.cfgPath))
	}
	if me.Name != "" {
		w = append(w, "--"+AsFlag, shellQuote(me.Name))
	}
	w = append(w, LoopBoard, boardEscalateName)
	if asked != "" {
		w = append(w, "--"+askedFlag, shellQuote(asked))
	}
	return strings.Join(append(w, shellQuote(ref)), " "), nil
}

// beekeeperAs is args run as the caller under the configuration.
func (a *app) beekeeperAs(me state.Party, args ...string) []string {
	out := []string{}
	if a.cfgPath != "" {
		out = append(out, "--"+configFlag, a.cfgPath)
	}
	if me.Name != "" {
		out = append(out, "--"+AsFlag, me.Name)
	}
	return append(out, args...)
}

// recheck adds e's re-check timer: board escalate looks at it again after
// every, knowing the State a note was filed on when asked is set.
func (a *app) recheck(ctx context.Context, me state.Party, e escalation, every time.Duration, asked string) error {
	run, err := a.escalateRun(me, e.Ref, asked)
	if err != nil {
		return err
	}
	return a.beekeeperStep(ctx, "timer add", e.Ref, a.beekeeperAs(me, LoopTimer, LoopAdd, every.String(), escalateWhat+e.Ref, "--run", run))
}

// beekeeperStep runs one beekeeper command for ref and makes its failure
// an error.
func (a *app) beekeeperStep(ctx context.Context, name, ref string, args []string) error {
	out, code, err := BeekeeperRun(ctx, args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return &exitError{code: code, msg: fmt.Sprintf("beekeeper %s for %s: exit %d: %s", name, ref, code, oneLine(out))}
	}
	return nil
}

// escalate acts on a judged e: a skipped or unchanged item gets its
// re-check timer only; any other gets one note for the person, naming what
// it waits on, who, since when and the evidence, and a timer that runs
// board escalate on it again after every carrying the State the note was
// filed on: while that State holds, it is not asked again.
func (a *app) escalate(ctx context.Context, me state.Party, e escalation, every time.Duration) error {
	if e.Skip != "" {
		return a.recheck(ctx, me, e, every, e.Asked)
	}
	if e.Unchanged {
		return a.recheck(ctx, me, e, every, e.State)
	}
	person := a.cfg.Guide.Person
	if person == "" {
		return fmt.Errorf("guide.person is unset: no person to escalate %s to", e.Ref)
	}
	here, _, _ := strings.Cut(e.Ref, "#")
	linked := func(s string) string { return post.Linked(s, nil, here) }
	// The question is short and whole; the title and the evidence, which
	// may name issues as owner/repo#n, go to the status quo with their refs
	// linked, as a note for the person needs.
	question := fmt.Sprintf("What unblocks %s?", e.URL)
	quo := fmt.Sprintf("%s (%s) sits in Blocked on the board. Who: %s. Evidence: %s.", e.URL, linked(e.Title), linked(e.Who), linked(strings.Join(e.Evidence, "; ")))
	note := a.beekeeperAs(me, LoopNote, LoopAdd, "--for", person, "--ref", e.Ref, "--due", a.cfg.Supervisor.NoteDue.String(),
		"--status-quo", truncate(quo, 2900),
		"--why", "a blocked item gets no worker until what holds it is settled",
		"--default", fmt.Sprintf("it stays blocked; beekeeper looks again in %s and asks again only when what holds it changed", every),
		"--checked", fmt.Sprintf("beekeeper board escalate: the board's Status and when it was set, the recorded blocked-by issues, the assignees, the newest %d comments, the parked workers, and %s's open notes and recorded answers on %s", escalateComments, person, e.Ref),
		question)
	// The timer first: a note that fails after it is retried once its
	// timer fires, never on every tidy.
	if err := a.recheck(ctx, me, e, every, e.State); err != nil {
		return err
	}
	return a.beekeeperStep(ctx, "note add", e.Ref, note)
}

// escalateBoard is b read for board escalate: its order with a step of the
// blocked Status, so the read takes the blocked items whatever board.order
// names. The step only widens the read; escalate picks nothing by it.
func escalateBoard(b config.Board) config.Board {
	b.Order = append(slices.Clone(b.Order), config.BoardStep{Name: boardEscalateName, Status: []string{blockedStatus}})
	return b
}

// escalateAll acts on each of es: one item's failure never holds back the
// others. It returns the ones acted on and the failures joined.
func (a *app) escalateAll(ctx context.Context, me state.Party, es []escalation, every time.Duration) ([]escalation, error) {
	var done []escalation
	var errs []error
	for _, e := range es {
		if err := a.escalate(ctx, me, e, every); err != nil {
			errs = append(errs, err)
			continue
		}
		done = append(done, e)
	}
	return done, errors.Join(errs...)
}

// readEscalateComments reads an item's newest comments; a seam for the
// tests.
var readEscalateComments = func(ctx context.Context, ref string) ([]github.Comment, error) {
	return github.LatestComments(ctx, boardGH, ref, escalateComments)
}

// judgeAll reads each of es's facts and judges it.
func (a *app) judgeAll(ctx context.Context, st *state.State, es []escalation, asked string) ([]escalation, error) {
	if len(es) == 0 {
		return nil, nil
	}
	// An answer counts from when its item's Status was set: the oldest of
	// those bounds the read, unless one is unknown.
	from := es[0].Since
	for _, e := range es {
		if e.Since.IsZero() {
			from = time.Time{}
			break
		}
		if e.Since.Before(from) {
			from = e.Since
		}
	}
	if !from.IsZero() {
		// answeredSince reads after it; judge counts one at it too.
		from = from.Add(-time.Nanosecond)
	}
	answers, err := a.answeredSince(from)
	if err != nil {
		return nil, err
	}
	out := make([]escalation, 0, len(es))
	for _, e := range es {
		f := escalateFacts{People: a.cfg.Board.People, Person: a.cfg.Guide.Person, Asked: asked,
			Parked: agentsParkedOn(st, e.Ref, e.URL), Notes: st.Notes, Answers: answers}
		f.Comments, f.CommentsErr = readEscalateComments(ctx, e.Ref)
		out = append(out, a.judge(e, f))
	}
	return out, nil
}

// boardEscalateName is the board escalate subcommand.
const boardEscalateName = "escalate"

func (a *app) boardEscalateCmd() *cobra.Command {
	var asked string
	c := &cobra.Command{
		Use:   "escalate [<owner/repo#n>...]",
		Short: "Escalate the board's blocked items to the person with what holds them, once while they stay unchanged",
		Long: `Reads the board and looks at each open item in Blocked (Blocked / Waiting):
its Status and when it was set, its open recorded blockers, its assignees,
its newest comments (the tail of its conversation), the workers parked on
it, and the person's open notes and recorded answers (answer.recorded) on
its thread. It files no note, and prints why (skipped), for an item

  - assigned to anyone outside board.people: the wait is the assignee's;
  - with an open note for guide.person on it (--ref or --holds);
  - whose thread guide.person answered since its Status was set;
  - whose newest comment by a person comes from outside the organisation
    (a customer's): the wait is on them;
  - with an open recorded blocker (board block): board next offers it once
    its blockers close;
  - with a worker parked on it or on its task (a worker whose session
    serves it, or whose task names its URL, parked on its pull request, a
    hold or the GitHub gate): the wait is the worker's;
  - with a comment, written since its Status was set (or the day before),
    that names another issue or pull request: what holds it is recorded.

Any other gets one note for guide.person naming what it waits on, who, since
when and the evidence read. Every item looked at gets a timer
(re-check blocked <owner/repo#n>) that runs board escalate on it again after
board.escalateEvery (default 168h); while it is open the item is not looked
at again. A note's timer carries the state of the item it was filed on
(--asked): when it fires and the item is still blocked and unchanged (its
Status and when it was set, its open blockers, its assignees and its newest
comment by a person), it is not asked again, only re-checked. Once it left
Blocked nothing happens. An item whose recorded blockers all closed before
its Status was set is left out: the order's blocker-cleared step offers it
as work; one whose Status was set after is held by it and looked at. The
read takes the Blocked items whatever board.order names. With refs, only
those items are looked at.

The supervisor loop runs it every board.tidyEvery after board tidy; a
negative board.escalateEvery escalates nothing. A worker that finds its item
blocked records what holds it as it moves it there (board move <item>
blocked --by <issue>); a wait on a pull request, a hold or the GitHub gate is
a park, the item left where it is.
Prints one line per item looked at.`,
		RunE: func(cmd *cobra.Command, refs []string) error {
			every := a.cfg.Board.EscalateEvery.Duration
			if every < 0 {
				return nil
			}
			if asked != "" && len(refs) != 1 {
				return usageErr("--%s names the state of one item: give exactly one <owner/repo#n>", askedFlag)
			}
			me, _ := a.caller()
			cl := a.boardClient()
			cl.Board = escalateBoard(cl.Board)
			snap, err := cl.Read(cmd.Context(), a.now)
			if err != nil {
				return err
			}
			blocked, err := board.Resolve(board.StatusField, snap.Statuses, blockedStatus)
			if err != nil {
				return err
			}
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			judged, err := a.judgeAll(cmd.Context(), st, escalations(snap.Items, blocked, st.Timers, refs), asked)
			if err != nil {
				return err
			}
			done, failed := a.escalateAll(cmd.Context(), me, judged, every)
			if a.json {
				if err := a.printJSON(done); err != nil {
					return err
				}
				return failed
			}
			for _, e := range done {
				var line string
				switch {
				case e.Skip != "":
					line = fmt.Sprintf("skipped %s: %s; re-checked in %s", e.URL, e.Skip, every)
				case e.Unchanged:
					line = fmt.Sprintf("unchanged %s since its note: not asked again; re-checked in %s", e.URL, every)
				default:
					line = fmt.Sprintf("escalated %s (%s; who: %s): re-checked in %s", e.URL, e.Why, e.Who, every)
				}
				if _, err := fmt.Fprintln(a.out, line); err != nil {
					return err
				}
			}
			return failed
		},
	}
	c.Flags().StringVar(&asked, askedFlag, "", "the state of the item its last note was filed on (a re-check timer's run)")
	return c
}
