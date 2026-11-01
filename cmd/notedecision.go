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

	"github.com/giantswarm/beekeeper/internal/execution"
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
	// answerMax bounds a person's words of an answer, shown in the closed
	// decision's section.
	answerMax = 3000
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

// defaultedEvent is the note.defaulted event of n closed at its due time.
func defaultedEvent(by state.Party, n *state.Note) state.Event {
	return event(by, noteDefaulted, "#%d defaulted for %s: %s (asked by %s: %s)", n.ID, cmp.Or(n.For, "nobody named"), n.Default, n.By.Name, n.Text)
}

// answeredLine and defaultedLine are what the watch says of a closed
// decision and what its filer is told.
func answeredLine(id int, by, answer string) string {
	return fmt.Sprintf("NOTE ANSWERED #%d by %s: %s", id, by, answer)
}

// replacedLine is what the filer of a decision closed by its default is
// told of an answer that came after it.
func replacedLine(n *state.Note, by, answer string) string {
	return fmt.Sprintf("%s (replaces the default applied: %s)", answeredLine(n.ID, by, answer), oneLine(n.Default))
}

func defaultedLine(n *state.Note) string {
	return fmt.Sprintf("NOTE DEFAULTED #%d: %s", n.ID, n.Default)
}

// defaultDue takes the decisions out of st whose due time has come
// unanswered and returns them, to close with their default. Only a decision
// whose default is an action closes: a sign-in's due time is a reminder, and
// a decision filed before defaults had to act stays open.
func defaultDue(st *state.State, now time.Time) []state.Note {
	var due []state.Note
	st.Notes = slices.DeleteFunc(st.Notes, func(n state.Note) bool {
		if n.Due.IsZero() || now.Before(n.Due) || !isAction(n.Default) || n.Kind != noteDecision {
			return false
		}
		due = append(due, n)
		return true
	})
	return due
}

// defaultedKeep is how long a decision closed by its default stays
// answerable: an answer within it replaces the default.
const defaultedKeep = 24 * time.Hour

// closeDefaulted closes the decisions of st due unanswered with their
// default, keeps them answerable for defaultedKeep (st.Defaulted), drops
// the decisions kept longer, and returns their watch lines, their events and
// the notes.
func closeDefaulted(st *state.State, person string, by state.Party, now time.Time) ([]string, []state.Event, []state.Note) {
	due := defaultDue(st, now)
	st.Defaulted = slices.DeleteFunc(st.Defaulted, func(n state.Note) bool { return now.Sub(n.Due) > defaultedKeep })
	st.Defaulted = append(st.Defaulted, due...)
	lines := make([]string, 0, len(due))
	evs := make([]state.Event, 0, len(due))
	for i := range due {
		lines = append(lines, defaultedLine(&due[i]))
		evs = append(evs, defaultedEvent(by, &due[i]))
	}
	return lines, evs, due
}

// answerable is the note id an answer may answer: an open one, or one its
// default closed within defaultedKeep (defaulted true).
func answerable(st *state.State, id int, now time.Time) (n *state.Note, defaulted bool) {
	if i := slices.IndexFunc(st.Notes, func(o state.Note) bool { return o.ID == id }); i >= 0 {
		return &st.Notes[i], false
	}
	if i := slices.IndexFunc(st.Defaulted, func(o state.Note) bool { return o.ID == id }); i >= 0 && now.Sub(st.Defaulted[i].Due) <= defaultedKeep {
		return &st.Defaulted[i], true
	}
	return nil, false
}

// takeAnswered takes note id out of st's open or defaulted notes, and
// reports whether it was there and whether its default had closed it.
func takeAnswered(st *state.State, id int) (found, defaulted bool) {
	is := func(o state.Note) bool { return o.ID == id }
	if i := slices.IndexFunc(st.Notes, is); i >= 0 {
		st.Notes = slices.Delete(st.Notes, i, i+1)
		return true, false
	}
	if i := slices.IndexFunc(st.Defaulted, is); i >= 0 {
		st.Defaulted = slices.Delete(st.Defaulted, i, i+1)
		return true, true
	}
	return false, false
}

// tellFiler delivers line from by to the session that filed n, on the roster
// or off it (by its session id). A note filed in a role (the supervisor's, the guide's)
// goes to the role's holder now, as a message to the role does: the run that
// filed it may have relayed since. While no session holds the role, or its
// holder cannot be told, line waits for the role's next start. It returns
// what it says of a line that reached another than the filer, or waits, ""
// otherwise. A central instance has no sessions to tell: it queues line in
// the mailbox of the filer's person, addressed to the filing agent, whose
// machine's watch delivers it.
func (a *app) tellFiler(ctx context.Context, by state.Party, n *state.Note, line string) string {
	if a.central {
		if a.mailFiler == nil || n.By.Host == "" {
			return ""
		}
		if err := a.mailFiler(ctx, n, line); err != nil {
			return fmt.Sprintf("note #%d: telling %q on %s failed (%v): %s", n.ID, n.By.Name, n.By.Host, err, line)
		}
		return ""
	}
	if n.By.Name == "" {
		return ""
	}
	// The supervisor loop is no session: it reads the answer from the log
	// at its next pass.
	if n.By.Name == loopParty.Name && n.By.Session == "" {
		return fmt.Sprintf("note #%d: the supervisor loop acts on the answer at its next pass", n.ID)
	}
	quiet := *a
	quiet.executionRequestID = fmt.Sprintf("note/%d/%s", n.ID, execution.Digest([]byte(line)))
	a = &quiet
	st, err := a.store.Read()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "note #%d: telling %q failed (%v): %s\n", n.ID, n.By.Name, err, line)
		return ""
	}
	rl, ok := filedRole(st, n.By)
	if !ok {
		if err := a.quietWake(ctx, by, cmp.Or(wakeID(n.By), n.By.Name), line); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "note #%d: telling %q failed (%v): %s\n", n.ID, n.By.Name, err, line)
		}
		return ""
	}
	why := fmt.Sprintf("no session holds the %s's role", rl.name)
	if h := rl.get(st).Holder; h != nil {
		err := a.quietWake(ctx, by, cmp.Or(wakeID(h.Party), h.Name), line)
		switch {
		case err == nil && h.Is(n.By):
			return ""
		case err == nil:
			return fmt.Sprintf("note #%d: %q, which filed it, no longer holds the %s's role: told the %s, %q", n.ID, n.By.Name, rl.name, rl.name, h.Name)
		}
		why = fmt.Sprintf("the %s, %q, cannot be told (%v)", rl.name, h.Name, err)
	}
	if err := a.keepRoleLine(rl, line); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "note #%d: keeping the line for the %s failed (%v): %s\n", n.ID, rl.name, err, line)
		return ""
	}
	return fmt.Sprintf("note #%d: %s: the answer waits for its next start (beekeeper %s start)", n.ID, why, rl.name)
}

// keepRoleLine keeps line for rl's next holder, said at its start.
func (a *app) keepRoleLine(rl role, line string) error {
	return a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.RoleLines = append(st.RoleLines, state.RoleLine{Role: rl.name, Line: line, At: a.now.UTC()})
		return nil, nil
	})
}

// quietWake wakes the agent q with line from by, its own report discarded:
// it is not the command's.
func (a *app) quietWake(ctx context.Context, by state.Party, q, line string) error {
	quiet := *a
	quiet.out = io.Discard
	return wakeOwner(&quiet, ctx, by, q, line, "")
}

// filedRole is the role p filed its notes in: the one it holds, was relieved
// of, or whose run its name names.
func filedRole(st *state.State, p state.Party) (role, bool) {
	for _, rl := range roles {
		r := rl.get(st)
		if (r.Holder != nil && r.Holder.Is(p)) || relievedIn(r, p) != nil || rl.runOf(p.Name) > 0 {
			return rl, true
		}
	}
	return role{}, false
}

// takeRoleLines takes the lines waiting for rl's holder out of st, oldest
// first.
func takeRoleLines(st *state.State, rl role) []string {
	var lines []string
	st.RoleLines = slices.DeleteFunc(st.RoleLines, func(l state.RoleLine) bool {
		if l.Role != rl.name {
			return false
		}
		lines = append(lines, l.Line)
		return true
	})
	return lines
}
