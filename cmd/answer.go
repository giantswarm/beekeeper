package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/state"
)

// answerRecorded is the event of a person's words recorded on the thread
// they concern.
const answerRecorded = "answer.recorded"

// answerCmd records a person's words for the session that asked.
func (a *app) answerCmd() *cobra.Command {
	var noteID, choice int
	var via string
	var post bool
	c := &cobra.Command{
		Use:   "answer <owner/repo#n|URL> [<words>]",
		Short: "Record a person's answer, word for word, for the session it concerns",
		Long: `Record the person's words verbatim, however they came (--via cli or
slack), about the issue or pull request they concern: answer.recorded is
logged with the thread, the words and the channel they came through, which
beekeeper sets from where it runs (answer verify): one recorded from
inside an agent session is relayed, and the session told names its relay; --note <id> closes that note
(--choice <n> picks one of its options, 1-based, the words optional then)
and tells the session that filed it; without a note, the sessions serving
the thread (beekeeper sessions serve) are told. A decision its default
closed within the last day is still answered: the answer replaces the
default and its filer is told so. The words are at most 3000 characters.

Nothing is posted to GitHub: what shows there is the work's own result,
the merge, the reply to a reviewer, the issue update. --post also records
the decision as a comment, in plain words: who decided, the words, and the
issue or pull request it concerns by its URL. It goes to the thread itself
when its repository is private; a public one gets nothing, the comment
goes to the first private plan pull request (plans.repositories) whose
Epic line names the thread or an epic above it, then to the nearest epic
above it in a private repository. With neither the answer is refused,
naming what to add, nothing posted. Exit 3 when refused.

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
			return a.recordAnswer(cmd.Context(), refName(r), noteID, choice, via, strings.Join(args[1:], " "), post)
		},
	}
	c.Flags().IntVar(&noteID, "note", 0, "the note the answer answers: it closes, its filer is told")
	c.Flags().IntVar(&choice, "choice", 0, "the note's option chosen, 1-based")
	c.Flags().StringVar(&via, "via", viaCLI, "how the words came: cli or slack; the channel answer verify reads is set from where it runs")
	c.Flags().BoolVar(&post, "post", false, "also record the decision as a GitHub comment")
	c.AddCommand(a.answerVerifyCmd())
	return c
}

// recordedAnswer is an answer recorded: what it is about, what it answered
// and, posted, its thread and comment.
type recordedAnswer struct {
	About   string        `json:"about"`
	Thread  *board.Thread `json:"thread,omitempty"`
	Comment string        `json:"comment,omitempty"`
	By      string        `json:"by"`
	Via     string        `json:"via"`
	// Channel is how the answer reached beekeeper (answerChannel);
	// RelayedBy is who recorded one not the person's own.
	Channel   string `json:"channel"`
	RelayedBy string `json:"relayedBy,omitempty"`
	Answer    string `json:"answer"`
	Note      int    `json:"note,omitempty"`
	// Replaced is the default the answer replaces, of a decision its
	// default had closed.
	Replaced string   `json:"replacedDefault,omitempty"`
	Told     []string `json:"told,omitempty"`
}

// recordAnswer records words about about (owner/repo#n), answering note
// noteID when it is not 0: the event, the note closed, the asking session
// told and, with post, the comment.
func (a *app) recordAnswer(ctx context.Context, about string, noteID, choice int, via, words string, post bool) error {
	if via != viaCLI && via != viaSlack {
		return usageErr("--via %q is none of %s, %s", via, viaCLI, viaSlack)
	}
	me, err := a.caller()
	if err != nil {
		return err
	}
	var n *state.Note
	text := strings.TrimSpace(words)
	if n := utf8.RuneCountInString(text); n > answerMax {
		return usageErr("the answer has %d characters, at most %d", n, answerMax)
	}
	if noteID != 0 {
		st, err := a.store.Read()
		if err != nil {
			return err
		}
		if n, _ = answerable(st, noteID, a.now); n == nil {
			return refused("note #%d is not open, nor closed by its default within %s", noteID, shortDur(defaultedKeep))
		}
		if text, err = answerText(n, choice, text); err != nil {
			return err
		}
	} else if text == "" {
		return usageErr("an answer is the person's words")
	}
	rec := recordedAnswer{About: about, By: a.answerer(me, n), Via: via, Answer: text, Note: noteID}
	rec.Channel, rec.RelayedBy = a.answerChannel(me, via)
	if post {
		if err := a.postAnswer(ctx, &rec); err != nil {
			return err
		}
	}
	closedMeanwhile, wasOpen := false, false
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if n != nil {
			found, defaulted := takeAnswered(st, n.ID)
			closedMeanwhile, wasOpen = !found, found && !defaulted
			if defaulted {
				rec.Replaced = n.Default
			}
		}
		return []state.Event{answerEvent(me, &rec, n)}, nil
	})
	if err != nil {
		return fmt.Errorf("the answer is not logged%s: %w", rec.posted(), err)
	}
	if closedMeanwhile {
		return refused("the answer is logged%s, but note #%d closed meanwhile: its filer is not told", rec.posted(), n.ID)
	}
	line := rec.line(n)
	var notes []string
	if n != nil {
		rec.Told = []string{n.By.Name}
		if s := a.tellFiler(ctx, me, n, line); s != "" {
			notes = append(notes, s)
		}
		// A decision its default closed released its hold then.
		if wasOpen {
			notes = append(notes, a.release(ctx, me, n, closedAnswered)...)
		}
	} else {
		rec.Told, notes = a.tellServing(ctx, me, about, line)
	}
	if a.json {
		return a.printJSON(rec)
	}
	if _, err := fmt.Fprintf(a.out, "answer recorded for %s%s\n", about, rec.posted()); err != nil {
		return err
	}
	switch {
	case rec.Replaced != "":
		notes = append([]string{fmt.Sprintf("note #%d answered after its default: the answer replaces %q", n.ID, oneLine(rec.Replaced))}, notes...)
	case n != nil:
		notes = append([]string{fmt.Sprintf("note #%d answered and closed", n.ID)}, notes...)
	}
	for _, s := range notes {
		if _, err := fmt.Fprintln(a.out, s); err != nil {
			return err
		}
	}
	return nil
}

// postAnswer posts rec as a comment on its thread (board.AnswerThread),
// recording both in rec.
func (a *app) postAnswer(ctx context.Context, rec *recordedAnswer) error {
	cl := a.boardClient()
	t, err := cl.AnswerThread(ctx, rec.About)
	if r := (*board.Refusal)(nil); errors.As(err, &r) {
		return refused("%s", r.Reason)
	}
	if err != nil {
		return err
	}
	rec.Thread = t
	if rec.Comment, err = cl.Comment(ctx, t, answerBody(rec)); err != nil {
		return fmt.Errorf("the answer is not recorded, nothing posted on %s: %w", t.Ref, err)
	}
	return nil
}

// posted says where rec's comment is, "" when it is not posted.
func (r *recordedAnswer) posted() string {
	if r.Comment == "" {
		return ""
	}
	return ", posted at " + r.Comment
}

// line is what the asking session is told of the answer to n, nil for
// none.
func (r *recordedAnswer) line(n *state.Note) string {
	var s string
	switch {
	case n != nil && r.Replaced != "":
		s = fmt.Sprintf("%s (on %s)", replacedLine(n, r.attribution(), r.Answer), r.About)
	case n != nil:
		s = fmt.Sprintf("%s (on %s)", answeredLine(r.Note, r.attribution(), r.Answer), r.About)
	default:
		s = fmt.Sprintf("ANSWER on %s by %s: %s", r.About, r.attribution(), r.Answer)
	}
	if r.Comment != "" {
		s += " posted at " + r.Comment
	}
	return s
}

// answerer is who gave the answer: on a central instance the caller's
// verified email, which outlives the note on its answer.recorded event and
// in its NOTE ANSWERED line; else the person a note is for, a team's member
// the caller, else the guide's person.
func (a *app) answerer(me state.Party, n *state.Note) string {
	switch {
	case a.central && me.Person != "":
		return me.Person
	case n != nil && n.For != "" && !strings.HasPrefix(n.For, teamPrefix):
		return n.For
	case n != nil:
		return cmp.Or(me.Person, me.Name)
	}
	return cmp.Or(a.cfg.Guide.Person, me.Person, me.Name)
}

// answerBody is the comment recording rec for any reader of the thread:
// who decided, relayed by an agent when it was, the issue or pull request
// it concerns by its URL when the comment is on another thread, and the
// words verbatim as a quote. It names no note, session or channel: those
// mean something on one desk only.
func answerBody(rec *recordedAnswer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Decision** by %s", rec.By)
	if rec.RelayedBy != "" {
		b.WriteString(", relayed by an agent")
	}
	if t := rec.Thread; t != nil && t.Via != "" {
		fmt.Fprintf(&b, " on %s", cmp.Or(t.AboutURL, issueURL(t.About)))
	}
	b.WriteString(":\n\n")
	for line := range strings.Lines(rec.Answer) {
		b.WriteString("> " + strings.TrimRight(line, "\n") + "\n")
	}
	b.WriteString("\nWritten by an agent.")
	return b.String()
}

// issueURL is the URL of owner/repo#n: GitHub serves an issue's URL for a
// pull request too.
func issueURL(ref string) string {
	repo, n, _ := strings.Cut(ref, "#")
	return "https://github.com/" + repo + "/issues/" + n
}

// answerEvent is the answer.recorded event of rec: parseAnswered reads it
// back. Every field but the words is quoted or has no space, the words
// last: nothing they say reads as another field. "via" names the channel,
// one word, as a release before channels read it too.
func answerEvent(by state.Party, rec *recordedAnswer, n *state.Note) state.Event {
	at := ""
	if rec.Comment != "" {
		at = ", recorded at " + rec.Comment
	}
	note := ""
	if n != nil {
		note = fmt.Sprintf(", note #%d asked by %q: %q", n.ID, n.By.Name, n.Text)
	}
	return event(by, answerRecorded, "%s answered by %q via %s%s%s: %s", rec.About, rec.By, rec.Channel, at, note, rec.Answer)
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
		return nil, []string{fmt.Sprintf("no session serves %s: the answer is in the log", about)}
	}
	return told, []string{"told " + strings.Join(told, ", ")}
}
