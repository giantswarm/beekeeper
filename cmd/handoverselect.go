package cmd

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// What a hand-over selects: a successor reads what it acts on in its first
// minutes, and the rest stays one `handover --section` away.
const (
	// answeredWindow is how far back answered decisions are kept in view,
	// and how far back `note add` looks for a note asking one again.
	answeredWindow = 72 * time.Hour
	// endedKeep is how long a session record stays in the hand-over after
	// its session ended: long enough for the run that saw the end and its
	// successor to re-query the issue.
	endedKeep = time.Hour
)

// noteSplit is the open notes as a role's hand-over shows them: the pinned
// standing instructions, the role's own, and those the guide serves.
type noteSplit struct {
	Pinned, Own []state.Note
	// Guided counts the notes the guide serves by whom they are for.
	Guided map[string]int
}

// guided is the number of notes the guide serves.
func (s noteSplit) guided() int {
	n := 0
	for _, c := range s.Guided {
		n += c
	}
	return n
}

// splitNotes splits notes for the supervisor's hand-over: those filed for
// the guide's person or for the guide itself are the guide's.
func (a *app) splitNotes(notes []state.Note) noteSplit {
	s := noteSplit{Guided: map[string]int{}}
	for _, n := range notes {
		switch {
		case n.Pinned:
			s.Pinned = append(s.Pinned, n)
		case guides(a.cfg.Guide.Person, &n) || forGuide(&n):
			who, _ := noteFor(&n)
			if p := a.cfg.Guide.Person; strings.EqualFold(who, p) {
				who = p
			}
			s.Guided[who]++
		default:
			s.Own = append(s.Own, n)
		}
	}
	return s
}

// forGuide says whether n is filed for the guide role ("Guide", "Guide run
// 3: …") rather than for a person.
func forGuide(n *state.Note) bool {
	who, _ := noteFor(n)
	return strings.HasPrefix(strings.ToLower(who), strings.ToLower(guideRole.title))
}

// guidedText is the one line that stands for the guide's notes.
func guidedText(s noteSplit) string {
	whos := make([]string, 0, len(s.Guided))
	for who := range s.Guided {
		whos = append(whos, who)
	}
	slices.Sort(whos)
	parts := make([]string, len(whos))
	for i, who := range whos {
		parts[i] = fmt.Sprintf("%d for %s", s.Guided[who], who)
	}
	return fmt.Sprintf("%d notes the guide serves (%s): `beekeeper guide queue`, `beekeeper handover --section notes`", s.guided(), strings.Join(parts, ", "))
}

// liveRecords is the records a hand-over shows: the running sessions' and
// those ended within endedKeep; dropped counts the rest.
func (a *app) liveRecords(records []state.Record, sessions []*claude.Session) (kept []state.Record, dropped int) {
	for _, r := range records {
		_, live := claude.Live(sessions, r.Session)
		if !live && !r.Ended.IsZero() && a.now.Sub(r.Ended) > endedKeep {
			dropped++
			continue
		}
		kept = append(kept, r)
	}
	return kept, dropped
}

// droppedText is the line that stands for the records left out.
func droppedText(n int) string {
	return fmt.Sprintf("%d records of sessions ended over %s ago: `beekeeper handover --section records`", n, shortDur(endedKeep))
}

// shortDur is a whole duration without its zero minutes and seconds ("1h").
func shortDur(d time.Duration) string {
	s := d.String()
	s = strings.TrimSuffix(s, "0s")
	return strings.TrimSuffix(s, "0m")
}

// answered is a decision a person answered: the note.answered event read
// back.
type answered struct {
	ID       int
	For      string
	Answer   string
	Asker    string
	Question string
	At       time.Time
	By       string
}

// noteAnswered is the event of a person's answer on a note.
const noteAnswered = "note.answered"

// answeredDetail is note.answered's detail, as `note answer` writes it.
var answeredDetail = regexp.MustCompile(`(?s)^#(\d+) answered for (.*?): (.*?) \(asked by (.*?): (.*)\)$`)

// parseAnswered reads a note.answered event; ok is false for another one.
func parseAnswered(e state.Event) (answered, bool) {
	m := answeredDetail.FindStringSubmatch(e.Detail)
	if e.Verb != noteAnswered || m == nil {
		return answered{}, false
	}
	id, _ := strconv.Atoi(m[1])
	return answered{ID: id, For: m[2], Answer: m[3], Asker: m[4], Question: m[5], At: e.At, By: e.By.Name}, true
}

// answeredSince is the decisions answered since since, oldest first.
func (a *app) answeredSince(since time.Time) ([]answered, error) {
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == noteAnswered && e.At.After(since) })
	if err != nil {
		return nil, err
	}
	out := make([]answered, 0, len(evs))
	for _, e := range evs {
		if an, ok := parseAnswered(e); ok {
			out = append(out, an)
		}
	}
	return out, nil
}

// answers is what a hand-over shows of the answered decisions: all of
// answeredWindow, and since when the recent ones count (the predecessor's
// start, the last relay).
type answers struct {
	Since time.Time
	All   []answered
}

// Recent is the decisions answered since the last relay.
func (s answers) Recent() []answered {
	i := slices.IndexFunc(s.All, func(an answered) bool { return an.At.After(s.Since) })
	if i < 0 {
		return nil
	}
	return s.All[i:]
}

// handoverAnswers reads the answered decisions for a hand-over of st.
func (a *app) handoverAnswers(st *state.State) (answers, error) {
	floor := a.now.Add(-answeredWindow)
	all, err := a.answeredSince(floor)
	if err != nil {
		return answers{}, err
	}
	since, err := a.lastRelay(st, floor)
	return answers{Since: since, All: all}, err
}

// lastRelay is when the current supervisor's predecessor started: the
// latest supervisor.start before the holder's own, no earlier than floor.
func (a *app) lastRelay(st *state.State, floor time.Time) (time.Time, error) {
	before := a.now
	if st.Supervisor != nil {
		before = st.Supervisor.Since
	}
	starts, err := a.store.Events(0, func(e state.Event) bool {
		return e.Verb == supervisorRole.name+".start" && e.At.After(floor) && e.At.Before(before)
	})
	if err != nil || len(starts) == 0 {
		return floor, err
	}
	return starts[len(starts)-1].At, nil
}

// answeredText is an answered decision in a line: its answer in full, the
// question shortened to n runes (0: in full).
func (a *app) answeredText(an answered, n int) string {
	q := oneLine(an.Question)
	if n > 0 {
		q = truncate(q, n)
	}
	return fmt.Sprintf("#%d (for %s, answered %s): %q on: %s", an.ID, an.For, a.stamp(an.At), oneLine(an.Answer), q)
}

// repeats is the decision answered within answeredWindow that n asks
// again: for the same person, the same verb, on one of the same issues or
// PRs.
func repeats(recent []answered, n state.Note, question string) (*answered, string) {
	// Newest first: the latest answer is the one that stands.
	notes := make([]state.Note, len(recent))
	for i := range recent {
		j := len(recent) - 1 - i
		notes[i] = state.Note{ID: j, For: recent[j].For, Text: recent[j].Question}
	}
	o, ref := foldTarget(notes, n, question)
	if o == nil {
		return nil, ""
	}
	return &recent[o.ID], ref
}

// printAnswers lists the decisions answered since the last relay and
// counts the older ones of answeredWindow; all lists every one in full.
func (a *app) printAnswers(s answers, all bool) {
	list, n := s.Recent(), 160
	if all {
		list, n = s.All, 0
	}
	if len(list) == 0 {
		_, _ = fmt.Fprintf(a.out, "none answered since %s\n", a.stamp(s.Since))
	}
	for _, an := range list {
		_, _ = fmt.Fprintf(a.out, "- %s\n", a.answeredText(an, n))
	}
	if older := len(s.All) - len(list); older > 0 {
		_, _ = fmt.Fprintf(a.out, "%d more answered within %s: `beekeeper handover --section answers`\n", older, shortDur(answeredWindow))
	}
}

// printOwnNotes lists the role's own notes and the line for the guide's.
func (a *app) printOwnNotes(s noteSplit) {
	if len(s.Own) > 0 || s.guided() == 0 {
		a.printNotes(s.Own)
	}
	if s.guided() > 0 {
		_, _ = fmt.Fprintln(a.out, guidedText(s))
	}
}

// printPinned lists the pinned notes, the standing instructions.
func (a *app) printPinned(notes []state.Note) {
	if len(notes) == 0 {
		_, _ = fmt.Fprintln(a.out, "none pinned")
		return
	}
	a.printNotes(notes)
}

// printLiveRecords lists the records a hand-over shows and the line for the
// rest.
func (a *app) printLiveRecords(records []state.Record, sessions []*claude.Session) {
	kept, dropped := a.liveRecords(records, sessions)
	if len(kept) > 0 || dropped == 0 {
		a.printRecords(kept, sessions)
	}
	if dropped > 0 {
		_, _ = fmt.Fprintln(a.out, droppedText(dropped))
	}
}
