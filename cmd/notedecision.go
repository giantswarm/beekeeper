package cmd

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/giantswarm/beekeeper/internal/state"
)

// The limits a decision's message renders within (klaus-gateway's Slack
// blocks): the question is a header, the status quo a section, a label a
// button's neighbour and an option of a select.
const (
	questionMax  = 150
	statusQuoMax = 3000
	optionsMax   = 10
	labelMax     = 75
)

// The events of a decision's close by its default, and the ways an answer
// arrives.
const (
	noteDefaulted = "note.defaulted"
	viaCLI        = "cli"
	viaSlack      = "slack"
)

// teamPrefix opens a --for that names a team: team:<name>.
const teamPrefix = "team:"

// unrenderable names what keeps d from rendering as a decision's message:
// the question one line within questionMax, a status quo within
// statusQuoMax, at most optionsMax options with a label within labelMax
// each, a recommendation naming one of them, and a team named.
func (d noteDraft) unrenderable(forWho string) []string {
	var out []string
	q := strings.TrimSpace(d.Question)
	switch {
	case strings.ContainsAny(q, "\r\n"):
		out = append(out, "the question is one line; the detail goes into --status-quo")
	case utf8.RuneCountInString(q) > questionMax:
		out = append(out, fmt.Sprintf("the question has %d characters, at most %d; the detail goes into --status-quo", utf8.RuneCountInString(q), questionMax))
	}
	if strings.TrimSpace(d.StatusQuo) == "" {
		out = append(out, `--status-quo "<what is true now and why the question arises>"`)
	}
	if n := utf8.RuneCountInString(d.StatusQuo); n > statusQuoMax {
		out = append(out, fmt.Sprintf("--status-quo has %d characters, at most %d", n, statusQuoMax))
	}
	if len(d.Options) > optionsMax {
		out = append(out, fmt.Sprintf("%d options, at most %d", len(d.Options), optionsMax))
	}
	for _, o := range d.Options {
		label, _, _ := strings.Cut(o, ":")
		if n := utf8.RuneCountInString(strings.TrimSpace(label)); n > labelMax {
			out = append(out, fmt.Sprintf("--option %q: its label has %d characters, at most %d", o, n, labelMax))
		}
	}
	if d.Recommend < 0 || d.Recommend > len(d.Options) {
		out = append(out, fmt.Sprintf("--recommend %d names none of the %d options", d.Recommend, len(d.Options)))
	}
	if team, ok := strings.CutPrefix(forWho, teamPrefix); ok && strings.TrimSpace(team) == "" {
		out = append(out, "--for team:<name> names no team")
	}
	return out
}

// answerText is an answer word for word: the chosen option's label, the
// person's own words, or both.
func answerText(n *state.Note, choice int, text string) (string, error) {
	text = strings.TrimSpace(text)
	if choice == 0 {
		if text == "" {
			return "", usageErr("an answer is an option (--choice), the person's own words, or both")
		}
		return text, nil
	}
	label, _, ok := n.Option(choice)
	if !ok {
		return "", usageErr("note #%d has no option %d (it has %d)", n.ID, choice, len(n.Options))
	}
	if text == "" {
		return label, nil
	}
	return label + " — " + text, nil
}

// answeredEvent is the note.answered event of n's answer: parseAnswered
// reads it back.
func answeredEvent(by state.Party, n *state.Note, via, answer string) state.Event {
	return event(by, noteAnswered, "#%d answered for %s via %s: %s (asked by %s: %s)", n.ID, cmp.Or(n.For, "nobody named"), via, answer, n.By.Name, n.Text)
}

// defaultedEvent is the note.defaulted event of n closed at its due time.
func defaultedEvent(by state.Party, n *state.Note) state.Event {
	return event(by, noteDefaulted, "#%d defaulted for %s: %s (asked by %s: %s)", n.ID, cmp.Or(n.For, "nobody named"), n.Default, n.By.Name, n.Text)
}

// answeredLine and defaultedLine are what the watch says of a closed
// decision and what its filer is told.
func answeredLine(id int, by, answer string) string {
	return fmt.Sprintf("NOTE ANSWERED #%d by %s: %s", id, by, answer)
}

func defaultedLine(n *state.Note) string {
	return fmt.Sprintf("NOTE DEFAULTED #%d: %s", n.ID, n.Default)
}

// defaultDue takes the decisions out of st whose due time has come
// unanswered and returns them, to close with their default. Only a decision
// whose default is an action closes: a memo's or a sign-in's due time is a
// reminder, and a decision filed before defaults had to act stays open.
func defaultDue(st *state.State, person string, now time.Time) []state.Note {
	var due []state.Note
	st.Notes = slices.DeleteFunc(st.Notes, func(n state.Note) bool {
		if n.Due.IsZero() || now.Before(n.Due) || !isAction(n.Default) || noteKind(person, &n) != noteDecision {
			return false
		}
		due = append(due, n)
		return true
	})
	return due
}

// closeDefaulted closes the decisions of st due unanswered with their
// default and returns their watch lines, their events and the notes.
func closeDefaulted(st *state.State, person string, by state.Party, now time.Time) ([]string, []state.Event, []state.Note) {
	due := defaultDue(st, person, now)
	lines := make([]string, 0, len(due))
	evs := make([]state.Event, 0, len(due))
	for i := range due {
		lines = append(lines, defaultedLine(&due[i]))
		evs = append(evs, defaultedEvent(by, &due[i]))
	}
	return lines, evs, due
}

// tellFiler delivers line from by to the session that filed n, when it is a
// registered agent; a central instance has no sessions to tell.
func (a *app) tellFiler(ctx context.Context, by state.Party, n *state.Note, line string) {
	if a.central || n.By.Name == "" {
		return
	}
	quiet := *a // the wake's own report is not the command's
	quiet.out = io.Discard
	if err := wakeOwner(&quiet, ctx, by, cmp.Or(n.By.Session, n.By.Name), line, ""); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "note #%d: telling %q failed (%v): %s\n", n.ID, n.By.Name, err, line)
	}
}
