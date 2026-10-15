package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/state"
)

// answerRecorded is the event of a person's words recorded on the thread
// they concern.
const answerRecorded = "answer.recorded"

// answerCmd records a person's words on the thread they concern.
func (a *app) answerCmd() *cobra.Command {
	var noteID, choice int
	var via string
	c := &cobra.Command{
		Use:   "answer <owner/repo#n|URL> [<words>]",
		Short: "Record a person's answer, word for word, on the issue or pull request it concerns",
		Long: `Record the person's words verbatim on the GitHub thread they concern,
however they came (--via cli or slack). They are posted as an
agent-written comment naming who answered, how it came and the note it
answers; answer.recorded is logged with the thread, the comment's URL and
the words; --note <id> closes that open note (--choice <n> picks one of its
options, 1-based, the words optional then) and tells the session that filed
it; without a note, the sessions serving the thread (beekeeper sessions
serve) are told.

The comment goes to the thread itself when its repository is private. A
public one gets nothing: the comment goes to the first private plan pull
request (plans.repositories) whose Epic line names the thread or an epic
above it, then to the nearest epic above it in a private repository. With
neither the answer is refused, naming what to add, nothing posted. Exit 3
when refused.

The hand-over's answers and note add's re-ask warning read answer.recorded
(beekeeper log --verb answer.recorded).`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, ok := parseRef(args[0])
			if !ok {
				return usageErr("%q: an issue or pull request is owner/repo#n or its URL", args[0])
			}
			if choice != 0 && noteID == 0 {
				return usageErr("--choice picks an option of a note: --note <id>")
			}
			return a.recordAnswer(cmd.Context(), refName(r), noteID, choice, via, strings.Join(args[1:], " "))
		},
	}
	c.Flags().IntVar(&noteID, "note", 0, "the open note the answer answers: it closes, its filer is told")
	c.Flags().IntVar(&choice, "choice", 0, "the note's option chosen, 1-based")
	c.Flags().StringVar(&via, "via", viaCLI, "how the answer came: cli or slack")
	return c
}

// recordedAnswer is an answer recorded: the thread it is on, its comment and
// what it answered.
type recordedAnswer struct {
	Thread  *board.Thread `json:"thread"`
	Comment string        `json:"comment"`
	By      string        `json:"by"`
	Via     string        `json:"via"`
	Answer  string        `json:"answer"`
	Note    int           `json:"note,omitempty"`
	Told    []string      `json:"told,omitempty"`
}

// recordAnswer records words on the thread about concerns (owner/repo#n),
// answering open note noteID when it is not 0: the comment, the event, the
// note closed and the asking session told.
func (a *app) recordAnswer(ctx context.Context, about string, noteID, choice int, via, words string) error {
	if via != viaCLI && via != viaSlack {
		return usageErr("--via %q is none of %s, %s", via, viaCLI, viaSlack)
	}
	me, err := a.caller()
	if err != nil {
		return err
	}
	var n *state.Note
	text := strings.TrimSpace(words)
	if noteID != 0 {
		st, err := a.store.Read()
		if err != nil {
			return err
		}
		i := slices.IndexFunc(st.Notes, func(o state.Note) bool { return o.ID == noteID })
		if i < 0 {
			return refused("note #%d is not open", noteID)
		}
		n = &st.Notes[i]
		if text, err = answerText(n, choice, text); err != nil {
			return err
		}
	} else if text == "" {
		return usageErr("an answer is the person's words")
	}
	cl := a.boardClient()
	t, err := cl.AnswerThread(ctx, about)
	if r := (*board.Refusal)(nil); errors.As(err, &r) {
		return refused("%s", r.Reason)
	}
	if err != nil {
		return err
	}
	rec := recordedAnswer{Thread: t, By: a.answerer(me, n), Via: via, Answer: text, Note: noteID}
	if rec.Comment, err = cl.Comment(ctx, t, answerBody(&rec, n)); err != nil {
		return fmt.Errorf("the answer is not recorded on %s: %w", t.Ref, err)
	}
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if n != nil {
			st.Notes = slices.DeleteFunc(st.Notes, func(o state.Note) bool { return o.ID == n.ID })
		}
		return []state.Event{answerEvent(me, &rec, n)}, nil
	})
	if err != nil {
		return fmt.Errorf("the answer is posted (%s), not logged: %w", rec.Comment, err)
	}
	line := rec.line()
	var notes []string
	if n != nil {
		rec.Told = []string{n.By.Name}
		if s := a.tellFiler(ctx, me, n, line); s != "" {
			notes = append(notes, s)
		}
	} else {
		rec.Told, notes = a.tellServing(ctx, me, about, line)
	}
	if a.json {
		return a.printJSON(rec)
	}
	where := t.Ref
	if t.Via != "" {
		where += " (" + t.Via + ")"
	}
	if _, err := fmt.Fprintf(a.out, "answer recorded on %s: %s\n", where, rec.Comment); err != nil {
		return err
	}
	if n != nil {
		notes = append([]string{fmt.Sprintf("note #%d answered and closed", n.ID)}, notes...)
	}
	for _, s := range notes {
		if _, err := fmt.Fprintln(a.out, s); err != nil {
			return err
		}
	}
	return nil
}

// About is the issue or pull request the answer concerns.
func (r *recordedAnswer) About() string { return r.Thread.About }

// line is what the asking session is told of the answer.
func (r *recordedAnswer) line() string {
	if r.Note != 0 {
		return fmt.Sprintf("%s (on %s: %s)", answeredLine(r.Note, r.By, r.Answer), r.About(), r.Comment)
	}
	return fmt.Sprintf("ANSWER on %s by %s: %s (%s)", r.About(), r.By, r.Answer, r.Comment)
}

// answerer is who gave the answer: the person a note is for, a team's
// member the caller, else the guide's person.
func (a *app) answerer(me state.Party, n *state.Note) string {
	switch {
	case n != nil && n.For != "" && !strings.HasPrefix(n.For, teamPrefix):
		return n.For
	case n != nil:
		return cmp.Or(me.Person, me.Name)
	}
	return cmp.Or(a.cfg.Guide.Person, me.Person, me.Name)
}

// answerBody is the comment recording rec: who answered, how it came, the
// note it answers, the words verbatim as a quote, and the thread it
// concerns when that is public.
func answerBody(rec *recordedAnswer, n *state.Note) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Answer** from %s via %s", rec.By, rec.Via)
	if n != nil {
		fmt.Fprintf(&b, " to note #%d (asked by %s: %s)", n.ID, n.By.Name, oneLine(n.Text))
	}
	if rec.Thread.Via != "" {
		fmt.Fprintf(&b, ", about %s", rec.About())
	}
	b.WriteString(":\n\n")
	for line := range strings.Lines(rec.Answer) {
		b.WriteString("> " + strings.TrimRight(line, "\n") + "\n")
	}
	b.WriteString("\nWritten by an agent (`beekeeper answer`).")
	return b.String()
}

// answerEvent is the answer.recorded event of rec: parseAnswered reads it
// back.
func answerEvent(by state.Party, rec *recordedAnswer, n *state.Note) state.Event {
	e := event(by, answerRecorded, "%s answered by %s via %s, recorded at %s: %s", rec.About(), rec.By, rec.Via, rec.Comment, rec.Answer)
	if n != nil {
		e.Detail += fmt.Sprintf(" (note #%d, asked by %s: %s)", n.ID, n.By.Name, n.Text)
	}
	return e
}

// tellServing tells the sessions serving about (sessions serve) line, and
// returns their names and what is said of the telling.
func (a *app) tellServing(ctx context.Context, by state.Party, about, line string) ([]string, []string) {
	if a.central {
		return nil, nil
	}
	st, err := a.store.Read()
	if err != nil {
		return nil, []string{fmt.Sprintf("the sessions serving %s are not told (%v)", about, err)}
	}
	var told []string
	for _, r := range st.Records {
		if ref, ok := parseRef(r.Issue); !ok || !strings.EqualFold(refName(ref), about) || !r.Ended.IsZero() {
			continue
		}
		if err := a.quietWake(ctx, by, cmp.Or(r.Session.Session, r.Session.Name), line); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "telling %q failed (%v): %s\n", r.Session.Name, err, line)
			continue
		}
		told = append(told, r.Session.Name)
	}
	if len(told) == 0 {
		return nil, []string{fmt.Sprintf("no session serves %s: the answer is on the thread and in the log", about)}
	}
	return told, []string{"told " + strings.Join(told, ", ")}
}
