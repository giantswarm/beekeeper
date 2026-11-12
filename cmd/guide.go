package cmd

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/feed"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) guideCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   guideRole.name,
		Short: "Start, stop or show the guide: the session that walks the person through their decisions",
		Long: `The guide is the session that walks the person through the decisions
waiting on them and takes their answers back to the sessions that asked.
The supervisor is the supervisor loop, no session. The guide has no grant
power outside the explicit, expiring controls in beekeeper emergency.

The role moves in two steps that leave no gap: the relay starts the next run, "Guide run N+1", as a fresh session, and its
start takes the role (` + "`beekeeper guide handover --prompt`" + ` is its prompt). ` + "`beekeeper guide watch`" + `
says GUIDE RELAY DUE once the guide's context reaches guide.relayAt.

Its queue is every open decision filed --for its person, guide.person
(` + "`beekeeper guide queue`" + `), and every session the desktop files as waiting
on its person;
` + "`beekeeper answer <owner/repo#n> --note <id> <answer>`" + ` records an answer word for
word on the thread it concerns and closes the note.

Without a subcommand, shows the guide and its context in tokens against
guide.relayAt (exit 3 when none runs, 4 in the session a relay relieved).`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return a.roleStatus(guideRole) },
	}
	var takeOver, force, newRun bool
	start := &cobra.Command{
		Use:   "start [--new]",
		Short: "Take the guide's role in a run beekeeper started, or start the next run (--new)",
		Long: `Without --new, make the calling session the guide: a "Guide run N" beekeeper
started (the guide's relay, the standby watch after a crash, --new or
beekeeper resume) takes the role, relayed to it or from a holder whose
session is gone; the guide itself takes it again after a restart of its
CLI. A session started by any other means is refused.

With --new, beekeeper starts the next "Guide run N" as a fresh session, the
person's "start the guide", from a terminal: in guide.dir, with the
configured skill, model and mode (agents.mode; a background run has Remote
Control), named by its run. It takes the role over from a guide whose
session runs no CLI (a reboot, a crash), or takes it when none is
recorded. A guide whose CLI runs is relieved only with --take-over; a run
that is starting already refuses it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if newRun {
				return a.runNewRun(cmd.Context(), guideRole, takeOver)
			}
			return a.runStart(cmd.Context(), guideRole, takeOver)
		},
	}
	start.Flags().BoolVar(&newRun, "new", false, "start the next guide run as a fresh session, which takes the role")
	start.Flags().BoolVar(&takeOver, "take-over", false, "replace a guide whose session still runs without its relay")
	stop := &cobra.Command{
		Use:   "stop",
		Short: "End the guide's role on purpose",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return a.runStop(guideRole, force) },
	}
	stop.Flags().BoolVar(&force, "force", false, "end another session's guide role")
	status := &cobra.Command{
		Use:   "status",
		Short: "Show the guide (exit 3 when none runs, 4 when a relay relieved you)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return a.roleStatus(guideRole) },
	}
	c.AddCommand(a.guideWorkCmd(), start, a.relayCmd(guideRole, guideRelayLong), stop, status, a.guideQueueCmd(), a.guideNextCmd(), a.guideHandoverCmd(), a.guideWatchCmd())
	return c
}

const guideRelayLong = `Start the next guide run, "Guide run N+1", as a fresh session and open
the relay to it. Its first turn runs ` + "`beekeeper guide start`" + `, which takes the
role, and ends; the standby watch then resumes it in its desktop CLI with
` + "`beekeeper guide handover --prompt`" + `. Until then you stay the guide. The relay
stays open for guide.relayTTL (default 15m), then expires and you simply
keep guiding; --cancel withdraws it earlier. The supervisor's role is not
touched. Once the successor has started, ` + "`beekeeper guide status`" + ` in your
session exits 4.`

// queueItem is one open decision for the person: a note filed --for a
// person, or a session the desktop files as waiting on its person.
type queueItem struct {
	Note *state.Note `json:"note,omitempty"`
	// Owner is the session that filed the note or waits; OwnerLive says
	// whether it runs.
	Owner     string `json:"owner"`
	OwnerLive bool   `json:"ownerLive"`
	// Waiting is what a waiting session needs, as the desktop recorded it.
	Waiting string `json:"waiting,omitempty"`
	// Key names the item in the guide's feed and in a delta read: the
	// note, or the session's waiting turn.
	Key string `json:"key"`
}

// forPrefix is the "[for <person>]" an older note carries at the start of
// its text instead of its For.
var forPrefix = regexp.MustCompile(`(?i)^\s*\[for\s+([^\]]*[^\]\s])\s*\]\s*`)

// noteFor is whom n is filed for, its For or else an older note's
// "[for <person>]" prefix, and its text without that prefix.
func noteFor(n *state.Note) (who, text string) {
	if n.For != "" {
		return n.For, n.Text
	}
	if m := forPrefix.FindStringSubmatch(n.Text); m != nil {
		return m[1], n.Text[len(m[0]):]
	}
	return "", n.Text
}

// personUnset is what the guide's queue and feed say once when
// guide.person is unset.
const personUnset = "guide.person is unset: every note filed --for anyone is shown"

// guides says whether the guide of person has n in its queue: n is filed
// for person, in any case; every note filed --for anyone when person is
// unset.
func guides(person string, n *state.Note) bool {
	if person == "" {
		return n.For != ""
	}
	who, _ := noteFor(n)
	return strings.EqualFold(who, person)
}

// noteAddressee is who a note filed --for who is for: guide.person when who is
// another name of theirs or the guide, who itself when the desk knows it (the
// supervisor is a role, read by the loop). A name nobody
// here goes by is refused: its note would reach no guide. With guide.person
// unset every note is the guide's. central says the note goes to the central
// instance, which judges its own names; a note kept on this machine does not.
func (a *app) noteAddressee(who string, central bool) (string, error) {
	person := a.cfg.Guide.Person
	if person == "" || strings.HasPrefix(who, teamPrefix) || strings.EqualFold(who, supervisorCommand) {
		return who, nil
	}
	email := personEmail(a.cfg.Serve, who)
	if strings.EqualFold(who, person) || strings.EqualFold(who, guideName) || board.ContainsFold(a.cfg.Guide.PersonNames, who) || email != "" && strings.EqualFold(email, personEmail(a.cfg.Serve, person)) {
		return person, nil
	}
	if email != "" || central || board.ContainsFold(a.cfg.Board.People, who) {
		return who, nil
	}
	return "", usageErr("--for %q names nobody on this desk, so no guide would see the note: the guide's person is %q (guide.person; guide.personNames adds other names of theirs), others are in serve.people, board.people, an email or team:<name>", who, person)
}

// guideQueue is the queue of the guide of person: the open notes filed for
// person in filing order, then the sessions whose record says they wait on
// person (`beekeeper sessions serve --waits "<person>: …"`), in record
// order, running or stopped since (its record's end, else its wait) after
// since; those stopped before since are stopped, apart. Only these explicit
// asks count: the desktop's summary of a turn reads a status report as an
// ask. The guide's own session, an archived one, a test and an agent that
// reported its task after the wait (agents idle) are left out.
func guideQueue(st *state.State, sessions []*claude.Session, person string, since time.Time) (q, stopped []queueItem) {
	for i := range st.Notes {
		n := &st.Notes[i]
		if !guides(person, n) {
			continue
		}
		_, live := claude.Live(sessions, n.By)
		q = append(q, queueItem{Note: n, Owner: n.By.Name, OwnerLive: live, Key: "note#" + strconv.Itoa(n.ID)})
	}
	guide := guideRole.get(st).Holder
	for i := range st.Records {
		r := &st.Records[i]
		ask, ok := waitsOn(person, r.Waits)
		if !ok || (guide != nil && guide.Is(r.Session)) || reported(st, r) {
			continue
		}
		owner := r.Session.Name
		s, live := claude.Live(sessions, r.Session)
		if live {
			if s.Aside() != "" {
				continue
			}
			owner = cmp.Or(s.Name, owner)
		} else if (&claude.Session{Name: owner}).Aside() != "" {
			continue
		}
		key := cmp.Or(r.Session.HostSession, r.Session.Session, r.Session.Name)
		it := queueItem{Owner: owner, OwnerLive: live, Waiting: ask, Key: "wait:" + key + ":" + r.Waits}
		if !live && cmp.Or(r.Ended, r.At).Before(since) {
			stopped = append(stopped, it)
			continue
		}
		q = append(q, it)
	}
	return q, stopped
}

// reported says whether r's session is an agent that reported its task
// after r's wait: finished (agents idle --done), or idle since.
func reported(st *state.State, r *state.Record) bool {
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(r.Session) })
	return i >= 0 && (st.Agents[i].Done || st.Agents[i].IdleSince.After(r.At))
}

// withCentral puts the central instance's notes for person into q after
// the machine's notes, before the sessions that wait: an owner runs here
// only when it filed from this machine, host.
func withCentral(q []queueItem, central []state.Note, sessions []*claude.Session, person, host string) []queueItem {
	var items []queueItem
	for i := range central {
		n := &central[i]
		if !guides(person, n) {
			continue
		}
		live := false
		if n.By.Host == "" || strings.EqualFold(n.By.Host, host) {
			_, live = claude.Live(sessions, n.By)
		}
		items = append(items, queueItem{Note: n, Owner: n.By.Name, OwnerLive: live, Key: "note:" + noteRef(n)})
	}
	at := slices.IndexFunc(q, func(it queueItem) bool { return it.Note == nil })
	if at < 0 {
		at = len(q)
	}
	return slices.Insert(q, at, items...)
}

// centralQueue is the guide's queue with the central instance's notes for
// its person; an unreachable central instance leaves them out with a
// warning on stderr.
func (a *app) centralQueue(stderr io.Writer, q []queueItem, central []state.Note, cerr error, sessions []*claude.Session) []queueItem {
	if cerr != nil {
		_, _ = fmt.Fprintf(stderr, "warning: %v\n", cerr)
	}
	return withCentral(q, central, sessions, a.cfg.Guide.Person, a.cfg.Identity.Host)
}

// waitsOn reads a record's --waits as an ask of person: "<person>: <ask>",
// person in any case, gives the ask and true; with person unset, every
// non-empty wait is one.
func waitsOn(person, waits string) (string, bool) {
	waits = strings.TrimSpace(waits)
	if waits == "" {
		return "", false
	}
	if person == "" {
		return waits, true
	}
	who, ask, ok := strings.Cut(waits, ":")
	if !ok || !strings.EqualFold(strings.TrimSpace(who), person) {
		return "", false
	}
	return cmp.Or(strings.TrimSpace(ask), waits), true
}

// guideQueue is the queue of the guide of the configured person, and the
// sessions waiting on it that stopped longer than guide.waitingTTL ago.
func (a *app) guideQueue(st *state.State, sessions []*claude.Session) (q, stopped []queueItem) {
	return guideQueue(st, sessions, a.cfg.Guide.Person, a.waitingSince(a.now))
}

// waitingSince is the stop time before which a waiting session folds at
// now: guide.waitingTTL before it, never with the TTL 0.
func (a *app) waitingSince(now time.Time) time.Time {
	if a.cfg.Guide.WaitingTTL.Duration <= 0 {
		return time.Time{}
	}
	return now.Add(-a.cfg.Guide.WaitingTTL.Duration)
}

// waiter is a waiting session's item owner: its name, "(stopped)" when it
// runs no CLI.
func waiter(q queueItem) string {
	if q.OwnerLive {
		return strconv.Quote(q.Owner)
	}
	return strconv.Quote(q.Owner) + " (stopped)"
}

// text is the item in one line: owner, status quo, deadline and default.
func (a *app) queueText(q queueItem) string {
	if q.Note == nil {
		return fmt.Sprintf("%s waits on its person: %s", waiter(q), oneLine(q.Waiting))
	}
	n := q.Note
	who, text := noteFor(n)
	owner := fmt.Sprintf("from %q", q.Owner)
	if !q.OwnerLive && (n.By.Session != "" || n.By.HostSession != "") { // a person or script does not end
		owner += " (ended)"
	}
	if n.Central != "" && n.By.Host != "" {
		owner += " on " + n.By.Host
	}
	s := fmt.Sprintf("%s for %s %s", noteRef(n), who, owner)
	if n.Kind == noteTodo {
		s = fmt.Sprintf("%s [todo] for %s %s", noteRef(n), who, owner)
	}
	if u, why, ok := noteDeferral(n); ok && a.now.Before(u) {
		s += ", deferred until " + clock(a.now, u)
		if why != "" {
			s += " (" + why + ")"
		}
	} else if !n.Due.IsZero() {
		s += ", due " + clock(a.now, n.Due)
		if !n.Due.After(a.now) {
			s += " (overdue)"
		}
	}
	s += ": " + oneLine(text)
	if n.Default != "" {
		s += "; if unanswered: " + oneLine(n.Default)
	}
	return s
}

func (a *app) guideQueueCmd() *cobra.Command {
	var full bool
	c := &cobra.Command{
		Use:   "queue",
		Short: "The decisions waiting on the person: its open notes and waiting sessions, with their owners",
		Long: `Every open decision filed for the guide's person, guide.person (note add
--for, or an older note's "[for <person>]" prefix, in any case), with the session
that filed it (its owner, and whether it still runs), its deadline and its
default, then every session that serves a wait on its person (sessions serve
--waits "<person>: …") with what it needs, while its CLI runs or stopped
less than guide.waitingTTL (default 2h) ago, except the guide's own session,
an archived one, a test (titled "test: …") and an agent that reported its
task after the wait (agents idle, or off the roster). The sessions stopped
longer ago fold into one line; --full lists them. With guide.person unset,
every note filed --for anyone, and a line that says so. A caller that has
read the queue before gets only what changed.

On a machine with a central instance (central.context), the central
instance's open notes for the person (note_list, through muster as the
person) follow the machine's, each named <context>#<n>; with the central
instance unreachable, a warning says so and the machine's queue stands.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := a.store.Peek()
			if err != nil {
				return err
			}
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			central, cerr := a.listCentralNotes()
			q, stopped := a.guideQueue(st, sessions)
			q = a.centralQueue(cmd.ErrOrStderr(), q, central, cerr, sessions)
			if a.json {
				if full {
					q = append(q, stopped...)
				}
				return a.printJSON(q)
			}
			facts := append(a.queueFacts(q), textFacts("stopped", a.stoppedLine(stopped))...)
			if a.cfg.Guide.Person == "" {
				facts = append(textFacts("person", personUnset), facts...)
			}
			return a.delta("guide-queue", full, facts, func() {
				if a.cfg.Guide.Person == "" {
					_, _ = fmt.Fprintln(a.out, personUnset)
				}
				a.printQueue(q)
				if l := a.stoppedLine(stopped); l != "" {
					_, _ = fmt.Fprintln(a.out, l)
				}
				if full {
					for _, it := range stopped {
						_, _ = fmt.Fprintln(a.out, "  "+a.queueText(it))
					}
				}
			})
		},
	}
	fullFlag(c, &full)
	return c
}

func (a *app) guideNextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "next",
		Short: "Serve the one decision to ask the person now, due first, and mark it as being asked",
		Long: `Serve one open decision for the guide's person and mark it as being asked:
the open note filed for the person that is due first (by due time, then the
undated in filing order). The same note is
served again until it is answered (` + "`beekeeper note answer <id> <answer>`" + `, its
thread's beekeeper answer) or
closed (note done); only then comes the next. On a machine with a central
instance (central.context), the central instance's open notes for the
person are served beside the machine's, named <context>#<n> and answered
there as the person (note answer <context>#<n>). Only the guide's session
serves decisions. With no open note it says so, and names how many sessions
wait on the person (` + "`beekeeper guide queue`" + `).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			central, cerr := a.listCentralNotes()
			var lines []string
			var item *queueItem
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				var evs []state.Event
				item, lines, evs, err = a.guideNext(st, sessions, me, func(q []queueItem) []queueItem {
					return a.centralQueue(cmd.ErrOrStderr(), q, central, cerr, sessions)
				})
				return evs, err
			})
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(item)
			}
			_, err = fmt.Fprintln(a.out, strings.Join(lines, "\n"))
			return err
		},
	}
}

// guideNext serves me, the guide, the decision to ask now (nextDecision),
// marks it in st and returns it with its lines and events; more, when set,
// adds to the machine's queue (the central instance's notes).
func (a *app) guideNext(st *state.State, sessions []*claude.Session, me state.Party, more func([]queueItem) []queueItem) (*queueItem, []string, []state.Event, error) {
	r := guideRole.get(st)
	if r.Holder == nil || !r.Holder.Is(me) {
		return nil, nil, nil, refused("only the guide serves its person's decisions: `beekeeper guide status` names it")
	}
	q, _ := a.guideQueue(st, sessions)
	if more != nil {
		q = more(q)
	}
	it := nextDecisionAt(q, r.AskingFrom+"#"+strconv.Itoa(r.Asking), a.now)
	if it == nil {
		r.Asking, r.AskingFrom = 0, ""
		guideRole.set(st, r)
		deferredCount, nextDue, waiting := 0, time.Time{}, 0
		for _, item := range q {
			if item.Note == nil {
				waiting++
				continue
			}
			if u, _, ok := noteDeferral(item.Note); ok && a.now.Before(u) {
				deferredCount++
				if nextDue.IsZero() || u.Before(nextDue) {
					nextDue = u
				}
			}
		}
		who := cmp.Or(a.cfg.Guide.Person, "the person")
		line := "no open decision for " + who
		if deferredCount > 0 {
			line = fmt.Sprintf("no decision currently due for %s; %d deferred note(s) wait in the queue (next due %s): beekeeper guide queue", who, deferredCount, clock(a.now, nextDue))
		}
		if waiting > 0 {
			line += fmt.Sprintf("; %d session(s) wait on their person: beekeeper guide queue", waiting)
		}
		return nil, []string{line}, nil, nil
	}
	n := *it.Note
	it.Note = &n
	ref := strings.TrimPrefix(noteRef(&n), "#")
	answerLine := fmt.Sprintf("record the answer word for word: beekeeper note answer %s \"<answer>\"; the next decision comes after it", ref)
	if len(n.Options) > 0 {
		answerLine = fmt.Sprintf("record the answer word for word: beekeeper note answer %s --choice <n> \"<answer>\" (or --keep-open / --defer <due> when the reply does not decide an option); the next decision comes after it", ref)
	}
	lines := []string{"asking " + a.queueText(*it), answerLine}
	if n.ID == r.Asking && n.Central == r.AskingFrom {
		lines[0] = "still " + lines[0]
		return it, lines, nil, nil
	}
	r.Asking, r.AskingFrom = n.ID, n.Central
	guideRole.set(st, r)
	return it, lines, []state.Event{event(me, "guide.asking", "%s %s", noteRef(&n), n.Text)}, nil
}

// nextDecision is the note the guide asks now: asking (its noteRef) while
// it is open in q and not deferred past now, else the non-deferred note in q
// due first, by due time, then the undated in filing order; nil when q holds
// no currently due note.
func nextDecisionAt(q []queueItem, asking string, now time.Time) *queueItem {
	var next *queueItem
	for i := range q {
		it := &q[i]
		switch {
		case it.Note == nil:
			continue
		case !now.IsZero() && noteDeferredAt(it.Note, now):
			continue
		case noteRef(it.Note) == asking:
			return it
		case next == nil, dueBefore(it.Note, next.Note):
			next = it
		}
	}
	return next
}

// dueBefore says whether n is due before o: an undated note is due last.
func dueBefore(n, o *state.Note) bool {
	return !n.Due.IsZero() && (o.Due.IsZero() || n.Due.Before(o.Due))
}

// stoppedLine is the one line that stands for the waiting sessions stopped
// longer than guide.waitingTTL ago, "" when there are none.
func (a *app) stoppedLine(stopped []queueItem) string {
	if len(stopped) == 0 {
		return ""
	}
	return fmt.Sprintf("%d stopped session(s) waited on %s, stopped over %s ago: beekeeper guide queue --full lists them",
		len(stopped), cmp.Or(a.cfg.Guide.Person, "their person"), strings.TrimSuffix(strings.TrimSuffix(a.cfg.Guide.WaitingTTL.String(), "0s"), "0m"))
}

func (a *app) printQueue(q []queueItem) {
	if len(q) == 0 {
		_, _ = fmt.Fprintln(a.out, "nothing waits on the person")
		return
	}
	for _, it := range q {
		_, _ = fmt.Fprintln(a.out, a.queueText(it))
	}
}

func (a *app) queueFacts(q []queueItem) []fact {
	facts := make([]fact, 0, len(q))
	for _, it := range q {
		l := "queue: " + a.queueText(it)
		facts = append(facts, fact{Key: it.Key, Sig: moving.ReplaceAllString(l, ""), Line: l})
	}
	return facts
}

func (a *app) guideHandoverCmd() *cobra.Command {
	var prompt, full bool
	c := &cobra.Command{
		Use:   "handover",
		Short: "Everything the next guide needs: the guide, its queue and an open relay",
		Long: `Print the guide's hand-over: the guide and its context in tokens, an open
relay and the queue. --prompt prints the successor's session prompt instead:
the configured instructions (guide.skill's bare slash command, "/guide you
take over …", or a reference to guide.instructions), the
board holds read from GitHub and the epics with their plans (as
beekeeper handover --section board-holds and --section epics show them; a
failed read is shown as not read),
the queue, an open relay and the commands that read the live values; no
other live value. The prompt references the desk's standing instructions
file; the ordinary hand-over carries standing.md in full. A caller that has read
the hand-over before gets only what changed; --full prints everything.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := a.store.Peek()
			if err != nil {
				return err
			}
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			q, _ := a.guideQueue(st, sessions)
			r := guideRole.get(st)
			s, err := a.readStanding()
			if err != nil {
				return err
			}
			decisions, err := a.decisionHistory("")
			if err != nil {
				return err
			}
			if prompt {
				return a.printGuidePrompt(r, q, s, a.boardHoldsHandover(cmd.Context(), st.Notes), st)
			}
			sv := readHolder(r, sessions, a.now, a.cfg.Guide.RestartGrace.Duration)
			head := "No guide is recorded."
			if v := a.viewRole(guideRole, r, sessions, sv); v != nil {
				head = fmt.Sprintf("Guide: %q since %s%s.", v.Name, clock(a.now, v.Since), v.contextText())
				if !v.Live {
					head = fmt.Sprintf("Guide: %q since %s, its session is gone.", v.Name, clock(a.now, v.Since))
				}
			}
			relay := a.relayLine(guideRole, r)
			subs := a.guideSubscriptionLines(cmd.Context(), st, sessions)
			unbacked := a.unbackedPersonItems(st, q, s)
			facts := append(textFacts("guide", head+"\n"+relay+"\n"+emergencyText(st, a.now)), a.queueFacts(q)...)
			if len(subs) > 0 {
				facts = append(facts, textFacts("subscriptions", strings.Join(subs, "\n"))...)
			}
			facts = append(facts, textFacts("decisions", decisionHistoryText(decisions))...)
			facts = append(facts, textFacts("guide-work", guideWorkText(st))...)
			facts = append(facts, textFacts("standing", a.capture(func() { a.printStanding(s) }))...)
			if len(unbacked) > 0 {
				facts = append(facts, textFacts("unqueued-open", strings.Join(unbacked, "\n"))...)
			}
			return a.delta("guide-handover", full, facts, func() {
				_, _ = fmt.Fprintf(a.out, "# Guide hand-over, %s\n\n%s\n", a.now.Format("2006-01-02 15:04 MST"), head)
				if relay != "" {
					_, _ = fmt.Fprintln(a.out, relay)
				}
				if len(subs) > 0 {
					_, _ = fmt.Fprintln(a.out, "\n## Subscription budgets")
					for _, l := range subs {
						_, _ = fmt.Fprintln(a.out, "- "+l)
					}
				}
				_, _ = fmt.Fprintln(a.out, "\n## Emergency grant\n"+emergencyText(st, a.now))
				_, _ = fmt.Fprintln(a.out, "\n## Standing instructions")
				a.printStanding(s)
				_, _ = fmt.Fprintln(a.out, "\n## Recorded decisions\n"+decisionHistoryText(decisions))
				_, _ = fmt.Fprintln(a.out, "\n## Queue")
				a.printQueue(q)
				if len(unbacked) > 0 {
					_, _ = fmt.Fprintln(a.out, "\n## Unqueued open-with-person items (warning)")
					_, _ = fmt.Fprintln(a.out, unbackedProseHeader)
					for _, w := range unbacked {
						_, _ = fmt.Fprintln(a.out, "- "+w)
					}
				}
				_, _ = fmt.Fprintln(a.out, "\n## Owned work, goals and restoration deadlines\n"+guideWorkText(st))
			})
		},
	}
	fullFlag(c, &full)
	c.Flags().BoolVar(&prompt, "prompt", false, "print the successor's session prompt: instructions, the queue and an open relay")
	return c
}

// relayLine says rl's open relay, "" when none is open.
func (a *app) relayLine(rl role, r state.Role) string {
	if !r.Relay.Open(a.now) {
		return ""
	}
	return fmt.Sprintf("Relayed to %q until %s: its `beekeeper %s start` takes the role.", r.Relay.To.Name, clock(a.now, r.Relay.Expires), rl.name)
}

// guideLiveCommands read what the guide's prompt leaves out.
var guideLiveCommands = [][2]string{
	{"beekeeper guide watch", "one line per new decision, waiting session, answer and guide relay: the source of a Monitor"},
	{"beekeeper guide next", "the one decision to ask now, due first; the next after its note answer"},
	{"beekeeper guide queue --full", "the decisions waiting on the person now, in full"},
	{"beekeeper handover --full", "all live desk state and recent history"},
	{"beekeeper guide status", "the guide and its context against guide.relayAt"},
	{"beekeeper board next", "the board's next item and epics in board order"},
	{"beekeeper log --verb note.", "the notes filed, answered and closed"},
}

func (a *app) printGuidePrompt(r state.Role, q []queueItem, s string, bv *boardHoldsView, st *state.State) error {
	intro, err := a.roleInstructions(guideRole, r)
	if err != nil {
		return err
	}
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
	decisions, err := a.decisionHistory("")
	if err != nil {
		return err
	}
	p("%s\n", intro)
	p("## Recorded decisions\n\n%s", decisionHistoryText(decisions))
	p("## Emergency grant\n\n%s\n", emergencyText(st, a.now))
	p("Check `beekeeper supervisor status` on takeover and at least every five minutes while pending work is held. When the loop holds every start or every merge for five minutes, use a covering active grant in that turn and report the command and outcome; act sooner on a known standstill. Carry its first-observed time and attempted recovery into the handover; relay never restarts the clock. Preserve the person's stops, explicit holds and CI gates. Relay needs no new words from the person; never extend the grant's deadline implicitly. Read `beekeeper answer history` before asking. Ask only when no grant covers the action, with a push notification through the configured notification channel, not background chat alone. Record notification failure as a problem and continue independent work.\n")
	p("## Pending state, %s\n", a.stamp(a.now))
	if s != "" {
		section(p, "Standing instructions", false, "")
		p("Read and follow %q.\n", a.standingPath())
	}
	p("## Owned work, goals and restoration deadlines\n\n%s\n", guideWorkText(st))
	if subs := a.guideSubscriptionLines(context.Background(), st, nil); len(subs) > 0 {
		section(p, "Subscription budgets", false, "")
		p("Keep every configured provider's subscription budget on pace; heavy underuse is a P1.\n")
		for _, l := range subs {
			p("- %s", l)
		}
		p("")
	}
	a.promptBoard(p, bv)
	if section(p, "Decisions waiting on the person", len(q) == 0, "None open.") {
		for _, it := range q {
			p("- %s", truncate(a.queueText(it), 320))
		}
		p("")
	}
	if unbacked := a.unbackedPersonItems(st, q, s); len(unbacked) > 0 {
		section(p, "Unqueued open-with-person items (warning)", false, "")
		p("%s", unbackedProseHeader)
		for _, w := range unbacked {
			p("- %s", w)
		}
		p("")
	}
	if l := a.relayLine(guideRole, r); section(p, "Relay", l == "", "None open.") {
		p("%s\n", l)
	}
	p("## Live values\n\nRead details when needed; earlier history stays in `beekeeper handover --full`:\n")
	for _, c := range guideLiveCommands {
		p("- `%s`: %s", c[0], c[1])
	}
	return nil
}

// guideSubscriptionLines reports every configured subscription provider's
// usage windows, pace, reset and underuse state, plus the next start choice.
func (a *app) guideSubscriptionLines(ctx context.Context, st *state.State, sessions []*claude.Session) []string {
	if a == nil || a.cfg == nil {
		return nil
	}
	k := a.liveCapacity()
	if len(k.Providers) > 0 {
		if st == nil && a.store != nil {
			st, _ = a.store.Peek()
		}
		if st == nil {
			st = &state.State{}
		}
		if sessions == nil && a.store != nil {
			sessions, _, _ = a.sessions()
		}
		a.syncAgyQuotaFromState(st, a.now)
		readings := make(map[string]capacity.ProviderReadingResult, len(k.Providers))
		anySubscription := false
		for _, name := range k.ProviderNames() {
			p := k.Providers[name]
			if p.SelfHosted() {
				continue
			}
			anySubscription = true
			if !p.Enabled() {
				continue
			}
			r, err := a.resolveReading(ctx, usageProviderFile(name), p.Usage)
			readings[name] = capacity.ProviderReadingResult{Reading: r, Err: err, OK: err == nil}
		}
		if !anySubscription {
			return nil
		}
		pa := countProviderAgents(st, sessions, k, a.now, a.cfg)
		providers := make(map[string]*capacity.ProviderView, len(k.Providers))
		var lines []string
		for _, name := range k.ProviderNames() {
			p := k.Providers[name]
			if p.SelfHosted() {
				providers[name] = capacity.BuildSelfHostedProviderView(name, p, pa[name], capacity.ProviderReadingResult{}, a.now)
				continue
			}
			pv := capacity.BuildSingleProviderView(name, p, pa[name], readings[name], a.now)
			providers[name] = pv
			line := name + ": " + pv.Line(a.now)
			if pv.Underused {
				if len(pv.Buckets) > 0 && !pv.SupplementalBuckets {
					var uParts []string
					for _, bName := range pv.BucketOrder {
						if bv := pv.Buckets[bName]; bv != nil && bv.Underused {
							uParts = append(uParts, bName+": "+capacity.UnderusedFigureText(bv.SevenDay, p.Usage, a.now))
						}
					}
					if len(uParts) > 0 {
						line += "; UNDERUSED (" + strings.Join(uParts, "; ") + ")"
					}
				} else if fig := capacity.UnderusedFigureText(pv.SevenDay, p.Usage, a.now); fig != "" {
					line += "; UNDERUSED (" + fig + ")"
				}
			}
			lines = append(lines, line)
		}
		if len(k.Choices) > 0 {
			cv := capacity.EvaluateChoices(k, providers, nil, a.noStart(st), nil, capacity.ForWorker, a.now)
			lines = append(lines, "choice: "+cv.Line())
		}
		return lines
	}
	if !k.Usage.Enabled() {
		return nil
	}
	_, u := a.resolveUsage(ctx, k)
	if u == nil {
		return nil
	}
	line := u.SnapshotLine(a.now)
	if u.Underused {
		if fig := capacity.UnderusedFigureText(u.SevenDay, k.Usage, a.now); fig != "" {
			line += "; UNDERUSED (" + fig + ")"
		}
	}
	return []string{line}
}

const unbackedProseHeader = "Prose notes are not tracked by `beekeeper guide queue`; verify each candidate item below and either back it in state (`beekeeper note reopen <id>`, `beekeeper note defer <id> --due <time>`, or `beekeeper note add --for <person> --kind todo`) or remove stale prose:"

var (
	openWithHeading  = regexp.MustCompile(`(?i)^#+\s*(?:still\s+)?(?:open\s+with|waiting\s+on|questions?\s+(?:open\s+)?for|pending\s+with)\b`)
	anyHeading       = regexp.MustCompile(`^#+\s+\S`)
	proseNoteMention = regexp.MustCompile(`(?i)\bnote\s+#?(\d+)\b`)
	proseOpenClaim   = regexp.MustCompile(`(?i)\b(?:still\s+open\s+with|open\s+with\s+\S+|unanswered\b|ask\s+with\s+the\b|waiting\s+on\s+you\b|needs\s+\S+'?s\s+review\b)`)
)

// unbackedPersonItems scans standing instructions, guide work, and the latest
// guide handover markdown note in guide.desk/guide.dir for candidate
// "open with the person" items that have no matching open note in q.
func (a *app) unbackedPersonItems(st *state.State, q []queueItem, standing string) []string {
	openIDs := map[int]bool{}
	openRefs := map[string]bool{}
	for _, it := range q {
		if it.Note == nil {
			continue
		}
		openIDs[it.Note.ID] = true
		for _, r := range a.noteNames(it.Note) {
			openRefs[strings.ToLower(r)] = true
		}
	}
	type source struct {
		name string
		text string
	}
	var sources []source
	if strings.TrimSpace(standing) != "" {
		sources = append(sources, source{name: standingFile, text: standing})
	}
	if st != nil {
		for _, w := range st.GuideWork {
			if strings.TrimSpace(w.Text) != "" {
				sources = append(sources, source{name: fmt.Sprintf("guide work #%d", w.ID), text: w.Text})
			}
		}
	}
	if path, text := a.latestGuideHandoverNote(); text != "" {
		sources = append(sources, source{name: path, text: text})
	}
	var out []string
	seen := map[string]bool{}
	add := func(msg string) {
		if !seen[msg] {
			seen[msg] = true
			out = append(out, msg)
		}
	}
	for _, src := range sources {
		inOpenSection := false
		for rawLine := range strings.Lines(src.text) {
			line := strings.TrimSpace(rawLine)
			if line == "" {
				continue
			}
			if anyHeading.MatchString(line) {
				inOpenSection = openWithHeading.MatchString(line)
				continue
			}
			if strings.EqualFold(line, "none") || strings.EqualFold(line, "none.") || strings.EqualFold(line, "- none") {
				continue
			}
			if !inOpenSection && !proseOpenClaim.MatchString(line) {
				continue
			}
			noteMatches := proseNoteMention.FindAllStringSubmatch(line, -1)
			namedRefs := namedRefs(line, a.cfg.Board.Owner)
			if len(noteMatches) > 0 {
				for _, m := range noteMatches {
					id, err := strconv.Atoi(m[1])
					if err == nil && !openIDs[id] {
						add(fmt.Sprintf("%s: mentions note #%d as open with the person, but note #%d is not in the open queue: %s", src.name, id, id, truncate(oneLine(line), 200)))
					}
				}
				continue
			}
			if len(namedRefs) > 0 {
				for _, ref := range namedRefs {
					if !openRefs[strings.ToLower(ref)] {
						add(fmt.Sprintf("%s: mentions %s as open with the person with no matching open note in the queue: %s", src.name, ref, truncate(oneLine(line), 200)))
					}
				}
				continue
			}
			if inOpenSection {
				add(fmt.Sprintf("%s: unbacked open-with-person prose item (no matching open note in the queue): %s", src.name, truncate(oneLine(line), 200)))
			}
		}
	}
	return out
}

func (a *app) latestGuideHandoverNote() (string, string) {
	var dirs []string
	for _, raw := range []string{a.cfg.Guide.Desk, a.cfg.Guide.Dir} {
		d := strings.TrimSpace(raw)
		if d == "" {
			continue
		}
		expanded := homePath(d)
		for _, candidate := range []string{filepath.Join(expanded, "notes"), expanded} {
			if !slices.Contains(dirs, candidate) {
				dirs = append(dirs, candidate)
			}
		}
	}
	bestRun := -1
	var bestPath string
	var bestMod time.Time
	runPattern := regexp.MustCompile(`(?i)^handover-guide(?:-run-(\d+))?\.md$`)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			m := runPattern.FindStringSubmatch(e.Name())
			if m == nil {
				continue
			}
			run := 0
			if m[1] != "" {
				run, _ = strconv.Atoi(m[1])
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if run > bestRun || (run == bestRun && info.ModTime().After(bestMod)) {
				bestRun = run
				bestMod = info.ModTime()
				bestPath = filepath.Join(dir, e.Name())
			}
		}
	}
	if bestPath == "" {
		return "", ""
	}
	raw, err := os.ReadFile(bestPath) //nolint:gosec // configured guide.desk or guide.dir handover markdown file
	if err != nil {
		return "", ""
	}
	return filepath.Base(bestPath), string(raw)
}

func (a *app) guideWatchCmd() *cobra.Command {
	var once bool
	c := &cobra.Command{
		Use:   "watch [--once]",
		Short: "The guide's feed: one line per new decision, waiting session, answer and guide relay",
		Long: `One line, once, for each open note newly filed for the guide's person
(the notes ` + "`beekeeper guide queue`" + ` lists), each session the desktop newly
files as waiting on its person (read from its session record, never from a
transcript), each note of the queue answered or closed, each unfiled problem the supervisor
settled (GUIDE PROBLEM, the full report to file in its owning repository), and the guide's
relay: GUIDE RELAY DUE at guide.relayAt, taken or
expired, and a restart of its CLI. Silent otherwise. What it said is kept
in the state, so a restarted feed says nothing again; only a standing
GUIDE RELAY DUE is said by each feed and repeats every five minutes until
a relay opens, and after a change of guide.relayAt. Runs until killed; --once polls once.

The feed re-executes itself after self-update or rollback replaces its binary.
A failed re-execution is reported once with the command that re-arms it. A save a newer release refuses (the state
written by it) is said as GUIDE SAVE REFUSED with the same command and the
lines that save would have recorded, again only when those lines change.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			changes, closeChanges := state.Changes(ctx, filepath.Join(a.store.Dir(), "state.json"), filepath.Join(a.store.Dir(), "events.jsonl"))
			defer closeChanges()
			tick := time.NewTicker(a.cfg.Watch.Interval.Duration)
			defer tick.Stop()
			if a.cfg.Guide.Person == "" {
				_, _ = fmt.Fprintln(a.out, time.Now().Format("15:04:05")+" GUIDE: "+personUnset)
			}
			if !once {
				a.guideDues = relayDues{}
			}
			self := newGuideSelf()
			for {
				a.now = time.Now()
				// Re-exec before consuming any durable feed entries.
				for _, l := range self.refresh(ctx) {
					_, _ = fmt.Fprintln(a.out, a.now.Format("15:04:05")+" "+l)
				}
				feed, err := a.guideFeed(ctx)
				for _, l := range self.lines(ctx, feed, err) {
					_, _ = fmt.Fprintln(a.out, a.now.Format("15:04:05")+" "+l)
				}
				if once {
					return nil
				}
				select {
				case <-ctx.Done():
					return nil
				case <-tick.C:
				case <-changes:
					// Coalesce a transaction's outbox writes before rereading.
					if !pauseChange(ctx) {
						return nil
					}
				}
			}
		},
	}
	c.Flags().BoolVar(&once, "once", false, "poll once and exit")
	return c
}

// guideFeed is one poll of the guide's feed: the lines it says, with the
// state written only when something is new.
func (a *app) guideFeed(ctx context.Context) ([]string, error) {
	sessions, _, err := a.sessions()
	if err != nil {
		return nil, err
	}
	st, err := a.store.Peek()
	if err != nil {
		return nil, err
	}
	// Outside the lock: the transcript's context and the log's answers.
	c := roleContext(guideRole.get(st), sessions, a.now, a.cfg.Guide.RelayAt, a.guideDues)
	q := quietness{checked: c > 0, context: c, relayAt: a.cfg.Guide.RelayAt}
	idleQ := quietness{}
	if !q.checked {
		idleQ = idleRoleReading(guideRole.get(st), sessions, a.now, a.cfg.Guide.RelayAt, a.guideDues)
	}
	problems, err := a.store.Follow("guide-problems", func(e state.Event) bool { return e.Verb == loop.LoopFileVerb })
	if err != nil {
		return nil, err
	}
	closed, err := a.closedNotes(st)
	if err != nil {
		return nil, err
	}
	central, centralErr := a.guideCentral(ctx, st)
	var archived map[string]bool
	if sessionLinked(st) {
		archived = claude.Archived(a.cfg)
	}
	fire := func(st *state.State) ([]string, []state.Event, bool) {
		seen, ce := guideRole.observeCLI(st, sessions, a.now)
		var lines []string
		for _, e := range ce {
			lines = append(lines, fmt.Sprintf("GUIDE RESTARTED: %q, %s; it keeps the role", e.By.Name, e.Detail))
		}
		rl, re := guideRole.fireRelay(st, a.now)
		reportsSince, workText, reportCount := st.GuideReportsSince, guideWorkText(st), len(st.GuideReports)
		pl, pf := guideProblemLines(st, guideReportEvents(st, problems, a.now))
		pf = pf || !reportsSince.Equal(st.GuideReportsSince) || workText != guideWorkText(st) || reportCount != len(st.GuideReports)
		fl, fed := a.feedLines(st, sessions, closed, findOrphaned(st, archived))
		cl, cf := a.centralGuideLines(st, sessions, central.notes, central.closed, centralErr == nil)
		lines = append(lines, cl...)
		fed = fed || cf
		lines = append(lines, pl...)
		fed = fed || pf
		lines = append(append(lines, rl...), fl...)
		reading := q
		if !reading.checked && len(lines) > 0 {
			reading = idleQ
		}
		dl, de := guideRole.fireRelayDue(st, reading, a.guideDues, a.now)
		lines = append(lines, dl...)
		evs := append(append(ce, re...), de...)
		heartbeat := st.Guide != nil && a.now.Sub(st.Guide.WatchSeen) >= 30*time.Second
		if heartbeat {
			st.Guide.WatchSeen = a.now.UTC()
		}
		return lines, evs, heartbeat || seen || fed || len(lines) > 0 || len(evs) > 0
	}
	// Preserve the read-only preview when the writer fence refuses the callback.
	lines, _, changed := fire(st)
	if !changed {
		return nil, ctx.Err()
	}
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		var evs []state.Event
		lines, evs, _ = fire(st)
		return evs, nil
	})
	return lines, err
}

type guideCentralSnapshot struct {
	notes  []state.Note
	closed map[int]state.Event
}

// guideCentral reads outside the state lock. A failed read preserves delivery
// marks until the central instance can establish which notes remain open.
func (a *app) guideCentral(ctx context.Context, st *state.State) (out guideCentralSnapshot, err error) {
	me, _ := a.caller()
	out.notes, err = a.centralNotes(ctx, me)
	if err != nil || !a.centralNotesOn() {
		return
	}
	missing := false
	for _, k := range st.GuideRole().Fed {
		if strings.HasPrefix(k, "note:"+a.cfg.Central.Context+"#") && !slices.ContainsFunc(out.notes, func(n state.Note) bool { return k == "note:"+noteRef(&n) }) {
			missing = true
		}
	}
	if !missing {
		return
	}
	var f feed.Feed
	if _, err = a.callCentralRaw(ctx, me, nameReadFeed, map[string]any{}, &f); err != nil {
		return
	}
	out.closed = map[int]state.Event{}
	for _, ev := range f.Events {
		detail, ok := strings.CutPrefix(ev.Line, feed.Line(ev.Kind, ev.Actor, ""))
		if !ok || !noteClosing(ev.Kind) {
			continue
		}
		id, _, _ := strings.Cut(strings.TrimPrefix(detail, "#"), " ")
		e := state.Event{Verb: ev.Kind, Detail: detail, By: state.Party{Name: ev.Actor.Name}}
		n, parseErr := strconv.Atoi(id)
		if an, ok := parseAnswered(e); ok {
			n, parseErr = an.ID, nil
		}
		if parseErr == nil {
			out.closed[n] = e
		}
	}
	return
}

func (a *app) centralGuideLines(st *state.State, sessions []*claude.Session, notes []state.Note, closed map[int]state.Event, available bool) (lines []string, changed bool) {
	if !available {
		return
	}
	guideRole.update(st, func(r *state.Role) {
		before := slices.Clone(r.Fed)
		var current []string
		for _, it := range withCentral(nil, notes, sessions, a.cfg.Guide.Person, a.cfg.Identity.Host) {
			current = append(current, it.Key)
			if !slices.Contains(r.Fed, it.Key) {
				lines = append(lines, "GUIDE DECISION: "+truncate(a.queueText(it), 240))
			}
		}
		r.Fed = slices.DeleteFunc(r.Fed, func(k string) bool {
			if !strings.HasPrefix(k, "note:"+a.cfg.Central.Context+"#") || slices.Contains(current, k) {
				return false
			}
			if slices.ContainsFunc(notes, func(n state.Note) bool { return k == "note:"+noteRef(&n) }) {
				return false
			}
			ref := strings.TrimPrefix(k, "note:")
			_, id, _ := strings.Cut(ref, "#")
			n, _ := strconv.Atoi(id)
			e, ok := closed[n]
			switch {
			case ok && e.Verb == answerRecorded:
				lines = append(lines, fmt.Sprintf("GUIDE ANSWERED (%s): %s: %s", truncate(e.By.Name, 30), ref, truncate(oneLine(e.Detail), 240)))
			case ok && e.Verb == noteDefaulted:
				lines = append(lines, "GUIDE DEFAULTED: "+ref+": "+truncate(oneLine(e.Detail), 240))
			case ok && e.Verb == noteOvertaken:
				lines = append(lines, "GUIDE CLOSED "+ref+" overtaken: "+truncate(oneLine(overtakenReason(e)), 200))
			default:
				lines = append(lines, "GUIDE CLOSED: note "+ref)
			}
			return true
		})
		for _, k := range current {
			if !slices.Contains(r.Fed, k) {
				r.Fed = append(r.Fed, k)
			}
		}
		slices.Sort(r.Fed)
		changed = !slices.Equal(before, r.Fed)
	})
	return
}

// closedNotes are the events that closed the notes the feed reported and
// that are no longer open, by note id: their answer, their done, or their
// overtaken.
func (a *app) closedNotes(st *state.State) (map[int]state.Event, error) {
	gone := map[int]bool{}
	for _, k := range st.GuideRole().Fed {
		if id, err := strconv.Atoi(strings.TrimPrefix(k, "note#")); err == nil && !slices.ContainsFunc(st.Notes, func(n state.Note) bool { return n.ID == id }) {
			gone[id] = true
		}
	}
	out := map[int]state.Event{}
	if len(gone) == 0 {
		return out, nil
	}
	evs, err := state.FollowSnapshot(a.store, "guide-closed-notes", "v1", func(e state.Event) bool {
		return e.Verb == answerRecorded || e.Verb == noteDefaulted || e.Verb == noteDone || e.Verb == noteOvertaken
	})
	if err != nil {
		return nil, err
	}
	for _, e := range evs {
		id, _, _ := strings.Cut(strings.TrimPrefix(e.Detail, "#"), " ")
		n, err := strconv.Atoi(id)
		if an, ok := parseAnswered(e); ok {
			n, err = an.ID, nil
		}
		if err == nil && gone[n] {
			out[n] = e // the latest wins
		}
	}
	return out, nil
}

// feedLines says each queue item the feed has not said yet, each orphaned
// note (findOrphaned) it has not said yet and each note it said that is
// closed now, and records what it said in the guide's Fed. It reports
// whether Fed changed.
func (a *app) feedLines(st *state.State, sessions []*claude.Session, closed map[int]state.Event, orphans []overtake) (lines []string, changed bool) {
	guideRole.update(st, func(r *state.Role) {
		var cur []string
		for _, k := range r.Fed {
			if strings.HasPrefix(k, "problem:") || strings.HasPrefix(k, "note:") {
				cur = append(cur, k)
			}
		}
		// A session that only aged out of the queue is no news.
		q, _ := a.guideQueue(st, sessions)
		for _, it := range q {
			k := it.Key
			cur = append(cur, k)
			if slices.Contains(r.Fed, k) {
				continue
			}
			if it.Note != nil {
				lines = append(lines, "GUIDE DECISION: "+truncate(a.queueText(it), 240))
			} else {
				lines = append(lines, fmt.Sprintf("GUIDE WAITING: %s needs its person: %s", waiter(it), truncate(oneLine(it.Waiting), 200)))
			}
		}
		for _, o := range orphans {
			i := slices.IndexFunc(st.Notes, func(n state.Note) bool { return n.ID == o.id })
			if i < 0 {
				continue
			}
			k := fmt.Sprintf("orphan#%d", o.id)
			cur = append(cur, k)
			if slices.Contains(r.Fed, k) {
				continue
			}
			who, text := noteFor(&st.Notes[i])
			lines = append(lines, fmt.Sprintf("GUIDE ORPHANED #%d for %s, %s; ask it, or close it with note done %d --overtaken: %s",
				o.id, who, o.reason, o.id, truncate(oneLine(text), 200)))
		}
		for _, k := range r.Fed {
			id, err := strconv.Atoi(strings.TrimPrefix(k, "note#"))
			if err != nil || slices.Contains(cur, k) || slices.ContainsFunc(st.Notes, func(n state.Note) bool { return n.ID == id }) {
				continue // still in the queue, or open and filed for someone else
			}
			switch e, ok := closed[id]; {
			case ok && e.Verb == answerRecorded:
				lines = append(lines, fmt.Sprintf("GUIDE ANSWERED (%s): %s", truncate(e.By.Name, 30), truncate(oneLine(e.Detail), 240)))
			case ok && e.Verb == noteDefaulted:
				lines = append(lines, "GUIDE DEFAULTED: "+truncate(oneLine(e.Detail), 240))
			case ok && e.Verb == noteOvertaken:
				lines = append(lines, fmt.Sprintf("GUIDE CLOSED #%d overtaken: %s", id, truncate(oneLine(overtakenReason(e)), 200)))
			default:
				lines = append(lines, fmt.Sprintf("GUIDE CLOSED: note #%d, without an answer", id))
			}
		}
		slices.Sort(cur)
		changed = !slices.Equal(cur, r.Fed)
		r.Fed = cur
	})
	return lines, changed
}

// guideProblemLines delivers reports once, escaped onto one physical line.
// Keys are retained only while their report remains in the event log.
func guideProblemLines(st *state.State, events []state.Event) (lines []string, changed bool) {
	workBeforeRecovery, reportsBeforeRecovery := guideWorkText(st), len(st.GuideReports)
	events = settleGuideRecoveries(st, events)
	recovered := workBeforeRecovery != guideWorkText(st) || reportsBeforeRecovery != len(st.GuideReports)
	current := map[string]bool{}
	for _, e := range events {
		key, text, ok := loop.Keyed(e.Detail)
		// Legacy file events summarized a note already delivered to the guide;
		// only the format carrying the actual report belongs in this feed.
		if ok && strings.Contains(text, loop.FileReportSeparator) {
			current["problem:"+key] = true
		}
	}
	guideRole.update(st, func(r *state.Role) {
		before := len(r.Fed)
		reportsBefore, workBefore := len(st.GuideReports), guideWorkText(st)
		r.Fed = slices.DeleteFunc(r.Fed, func(key string) bool {
			return strings.HasPrefix(key, "problem:") && !current[key]
		})
		changed = recovered || len(r.Fed) != before
		for _, e := range events {
			key, text, ok := loop.Keyed(e.Detail)
			if !ok || !strings.Contains(text, loop.FileReportSeparator) || !current["problem:"+key] || slices.Contains(r.Fed, "problem:"+key) {
				continue
			}
			if r.Holder != nil {
				if !retainGuideReport(st, key, text, e.At, r.Holder.Party) {
					continue
				}
			}
			r.Fed = append(r.Fed, "problem:"+key)
			lines = append(lines, "GUIDE PROBLEM: "+escapedLine(text))
			changed = true
		}
		changed = changed || reportsBefore != len(st.GuideReports) || workBefore != guideWorkText(st)
	})
	return lines, changed
}
