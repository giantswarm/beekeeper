package cmd

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
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

const recommendedSuffix = " (Recommended)"

// resolveNoteChoice keeps numeric choices in the note's stored order and
// accepts the note's own labels without relying on a rendered option order.
func resolveNoteChoice(n *state.Note, value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	if n != nil {
		for i := range n.Options {
			label, _, _ := n.Option(i + 1)
			if value == label {
				return i + 1, nil
			}
		}
	}
	if i, err := strconv.Atoi(value); err == nil {
		return i, nil
	}
	return 0, usageErr("--choice %q does not name an option label or a 1-based option number", value)
}

// answerText is an answer word for word: the chosen option's label, the
// person's own words, or both.
func answerText(n *state.Note, choice int, text string) (string, error) {
	text = strings.TrimSpace(text)
	if choice == 0 {
		if text == "" {
			return "", usageErr("an answer is an option (--choice), the person's own words, or both")
		}
		return normalizeOptionAnswer(n, text), nil
	}
	label, _, ok := n.Option(choice)
	if !ok {
		return "", usageErr("note #%d has no option %d (it has %d)", n.ID, choice, len(n.Options))
	}
	// A relay may add context, but must not name another option as its choice.
	for i, full := range n.Options {
		if i+1 == choice {
			continue
		}
		other, _, _ := n.Option(i + 1)
		if text != "" && trimOptionPrefix(other, text, full) != text {
			return "", usageErr("--choice %d selects %q, but the relayed text names %q", choice, label, other)
		}
	}
	text = trimOptionPrefix(label, text, n.Options[choice-1])
	if text == "" {
		return label, nil
	}
	return label + " — " + text, nil
}

// trimOptionPrefix removes a leading option label (with or without
// "(Recommended)" or its description) when text repeats the chosen option on
// its own or before " — ".
func trimOptionPrefix(label, text string, extraPrefixes ...string) string {
	text = strings.TrimSpace(text)
	base := strings.TrimSuffix(strings.TrimSpace(label), recommendedSuffix)
	prefixes := append([]string{label, base + recommendedSuffix, base}, extraPrefixes...)
	for _, p := range prefixes {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if text == p {
			return ""
		}
		if rest, ok := strings.CutPrefix(text, p+" — "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return text
}

// normalizeOptionAnswer normalizes "<label> (Recommended)" or
// "<label> (Recommended) — <notes>" (or the full "<label>: <desc> — <notes>")
// to use n's option label when n's label carries no "(Recommended)".
func normalizeOptionAnswer(n *state.Note, text string) string {
	text = strings.TrimSpace(text)
	if n == nil {
		return text
	}
	for i, full := range n.Options {
		label, _, _ := n.Option(i + 1)
		base := strings.TrimSuffix(label, recommendedSuffix)
		full = strings.TrimSpace(full)
		for _, p := range []string{base + recommendedSuffix, full} {
			if p == "" {
				continue
			}
			if text == base+recommendedSuffix {
				return label
			}
			if rest, ok := strings.CutPrefix(text, p+" — "); ok && strings.TrimSpace(rest) != "" {
				return label + " — " + strings.TrimSpace(rest)
			}
		}
	}
	return text
}

// noteOptionLabel reports whether choice and words record only an option
// label of n, without additional words from the person.
func noteOptionLabel(n *state.Note, choice int, words string) (string, bool) {
	if n == nil || len(n.Options) == 0 {
		return "", false
	}
	w := strings.TrimSpace(words)
	if choice > 0 {
		label, _, ok := n.Option(choice)
		if !ok || trimOptionPrefix(label, w, n.Options[choice-1]) != "" {
			return "", false
		}
		return cmp.Or(w, label), true
	}
	for i, full := range n.Options {
		label, _, _ := n.Option(i + 1)
		base := strings.TrimSuffix(label, recommendedSuffix)
		full = strings.TrimSpace(full)
		if w == label || w == base || w == base+recommendedSuffix || w == full {
			return w, true
		}
	}
	return "", false
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
// unanswered and returns them, to close with their default. A decision
// closes even when its legacy default is passive: a sign-in's due time
// remains a reminder. New decisions still require an action when filed.
func defaultDue(st *state.State, now time.Time) []state.Note {
	var due []state.Note
	st.Notes = slices.DeleteFunc(st.Notes, func(n state.Note) bool {
		if n.Due.IsZero() || now.Before(n.Due) || n.Kind != noteDecision {
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
// or off it (by its session id). A note filed in a role (the guide's)
// goes to the role's holder now, as a message to the role does: the run that
// filed it may have relayed since. While no session holds the role, or its
// holder cannot be told, line waits for the role's next start. It returns
// what it says of a line that reached another than the filer, or waits, ""
// otherwise. A central instance has no sessions to tell: it queues line in
// the mailbox of the filer's person, addressed to the filing agent, whose
// machine's watch delivers it.
func (a *app) tellFiler(ctx context.Context, by state.Party, n *state.Note, line string) string {
	return a.tellFilerWithWake(ctx, by, n, line, (*app).quietWake)
}

func (a *app) tellFilerWithWake(ctx context.Context, by state.Party, n *state.Note, line string, wake func(*app, context.Context, state.Party, string, string) error) (report string) {
	delivery := noteDelivery{Note: n.ID, Team: n.By.Team, Status: "delivered"}
	defer func() {
		// Hold-release messages use this route too; their delivery must not
		// replace the outcome of the answer notification itself.
		if !strings.HasPrefix(line, "NOTE ANSWERED #") && !strings.HasPrefix(line, "NOTE DEFAULTED #") {
			return
		}
		if err := a.recordNoteDelivery(by, delivery); err != nil {
			report += fmt.Sprintf(" note #%d: retaining notification status failed: %v", n.ID, err)
		}
	}()
	fail := func(err error) string {
		delivery.Status, delivery.Detail = noteDeliveryFailed, err.Error()
		return fmt.Sprintf("note #%d: notification failed (%v): %s", n.ID, err, line)
	}

	if a.central {
		if a.mailFiler == nil || n.By.Host == "" {
			return fail(fmt.Errorf("no mailbox route for the filing session"))
		}
		if err := a.mailFiler(ctx, n, line); err != nil {
			return fail(err)
		}
		delivery.Status = "queued"
		return ""
	}
	if n.By.Name == "" && wakeID(n.By) == "" {
		return fail(fmt.Errorf("the note retains no filing session identity"))
	}
	// The supervisor loop is no session: it reads the answer from the log
	// at its next pass.
	if n.By.Name == loopParty.Name && n.By.Session == "" {
		delivery.Status = noteDeliveryWaiting
		return fmt.Sprintf("note #%d: the supervisor loop acts on the answer at its next pass", n.ID)
	}
	quiet := *a
	quiet.executionRequestID = fmt.Sprintf("note/%d/%s", n.ID, execution.Digest([]byte(line)))
	a = &quiet
	st, err := a.store.Read()
	if err != nil {
		return fail(err)
	}
	rl, ok := filedRole(st, n.By)
	if !ok {
		if err := wake(a, ctx, by, cmp.Or(wakeID(n.By), n.By.Name), line); err != nil {
			return fail(err)
		}
		return ""
	}
	why := fmt.Sprintf("no session holds the %s's role", rl.name)
	if h := rl.get(st).Holder; h != nil {
		err := wake(a, ctx, by, cmp.Or(wakeID(h.Party), h.Name), line)
		switch {
		case err == nil && h.Is(n.By):
			return ""
		case err == nil:
			return fmt.Sprintf("note #%d: %q, which filed it, no longer holds the %s's role: told the %s, %q", n.ID, n.By.Name, rl.name, rl.name, h.Name)
		}
		why = fmt.Sprintf("the %s, %q, cannot be told (%v)", rl.name, h.Name, err)
	}
	if err := a.keepRoleLine(rl, line); err != nil {
		return fail(fmt.Errorf("keeping the line for the %s: %w", rl.name, err))
	}
	delivery.Status, delivery.Detail = noteDeliveryWaiting, why
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
	if by.Name == watchParty.Name {
		st, err := a.store.Read()
		if err != nil {
			return err
		}
		if wakeTargetPark(st, q) != nil {
			// The park's own settlement delivers its answer once. An
			// unrelated default or released hold must not start a turn.
			return nil
		}
	}
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
