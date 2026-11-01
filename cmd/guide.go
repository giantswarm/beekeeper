package cmd

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) guideCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   guideRole.name,
		Short: "Start, stop or show the guide: the session that walks the person through their decisions",
		Long: `The guide is the session that walks the person through the decisions
waiting on them and takes their answers back to the sessions that asked.
The supervisor is the supervisor loop, no session. The guide has no grant
power.

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
	var takeOver, force bool
	start := &cobra.Command{
		Use:   "start",
		Short: "Make the calling session the guide, or take the role relayed to it",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return a.runStart(cmd.Context(), guideRole, takeOver) },
	}
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
	c.AddCommand(start, a.relayCmd(guideRole, guideRelayLong), stop, status, a.guideQueueCmd(), a.guideNextCmd(), a.guideHandoverCmd(), a.guideWatchCmd())
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
	if !n.Due.IsZero() {
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
			st, err := a.store.Read()
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
	it := nextDecision(q, r.AskingFrom+"#"+strconv.Itoa(r.Asking))
	if it == nil {
		r.Asking, r.AskingFrom = 0, ""
		guideRole.set(st, r)
		waiting := len(q)
		line := "no open decision for " + cmp.Or(a.cfg.Guide.Person, "the person")
		if waiting > 0 {
			line += fmt.Sprintf("; %d session(s) wait on their person: beekeeper guide queue", waiting)
		}
		return nil, []string{line}, nil, nil
	}
	n := *it.Note
	it.Note = &n
	ref := strings.TrimPrefix(noteRef(&n), "#")
	lines := []string{"asking " + a.queueText(*it),
		fmt.Sprintf("record the answer word for word: beekeeper note answer %s \"<answer>\"; the next decision comes after it", ref)}
	if n.ID == r.Asking && n.Central == r.AskingFrom {
		lines[0] = "still " + lines[0]
		return it, lines, nil, nil
	}
	r.Asking, r.AskingFrom = n.ID, n.Central
	guideRole.set(st, r)
	return it, lines, []state.Event{event(me, "guide.asking", "%s %s", noteRef(&n), n.Text)}, nil
}

// nextDecision is the note the guide asks now: asking (its noteRef) while
// it is open in q, else the note in q due first, by due time, then the
// undated in filing order; nil when q holds no note.
func nextDecision(q []queueItem, asking string) *queueItem {
	var next *queueItem
	for i := range q {
		it := &q[i]
		switch {
		case it.Note == nil:
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
take over …", or guide.instructions), the
priorities and holds read from GitHub and the epics with their plans (as
beekeeper handover --prompt shows them; a failed read is shown as not read),
the queue, an open relay and the commands that read the live values; no
other live value. Both carry the desk's standing
instructions in full: standing.md in the state directory. A caller that has read
the hand-over before gets only what changed; --full prints everything.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := a.store.Read()
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
			if prompt {
				return a.printGuidePrompt(r, q, s, a.prioritiesHandover(cmd.Context(), st.Notes))
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
			facts := append(textFacts("guide", head+"\n"+relay), a.queueFacts(q)...)
			facts = append(facts, textFacts("standing", a.capture(func() { a.printStanding(s) }))...)
			return a.delta("guide-handover", full, facts, func() {
				_, _ = fmt.Fprintf(a.out, "# Guide hand-over, %s\n\n%s\n", a.now.Format("2006-01-02 15:04 MST"), head)
				if relay != "" {
					_, _ = fmt.Fprintln(a.out, relay)
				}
				_, _ = fmt.Fprintln(a.out, "\n## Standing instructions")
				a.printStanding(s)
				_, _ = fmt.Fprintln(a.out, "\n## Queue")
				a.printQueue(q)
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
	{"beekeeper guide queue", "the decisions waiting on the person now"},
	{"beekeeper guide status", "the guide and its context against guide.relayAt"},
	{"beekeeper priority", "the priority list now, with each epic's hold"},
	{"beekeeper log --verb note.", "the notes filed, answered and closed"},
}

func (a *app) printGuidePrompt(r state.Role, q []queueItem, s string, pv *prioritiesView) error {
	intro, err := a.roleInstructions(guideRole, r)
	if err != nil {
		return err
	}
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
	p("%s\n", intro)
	p("## Pending state, %s\n", a.stamp(a.now))
	promptStanding(p, s)
	a.promptPriorities(p, pv)
	if section(p, "Decisions waiting on the person", len(q) == 0, "None open.") {
		for _, it := range q {
			p("- %s", a.queueText(it))
		}
		p("")
	}
	if l := a.relayLine(guideRole, r); section(p, "Relay", l == "", "None open.") {
		p("%s\n", l)
	}
	p("## Live values\n\nRead them when you need them; this prompt holds none:\n")
	for _, c := range guideLiveCommands {
		p("- `%s`: %s", c[0], c[1])
	}
	return nil
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
GUIDE RELAY DUE is said once more, by each feed and after a change of
guide.relayAt. Runs until killed; --once polls once.

The feed never re-executes itself. Once self-update replaced its binary it
says GUIDE WATCH STALE, once, with the version it runs, the one installed
and the command that re-arms it. A save a newer release refuses (the state
written by it) is said as GUIDE SAVE REFUSED with the same command and the
lines that save would have recorded, again only when those lines change.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
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
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	// Outside the lock: the transcript's context and the log's answers.
	c := roleContext(guideRole.get(st), sessions, a.now, a.cfg.Guide.RelayAt, a.guideDues)
	q := quietness{checked: c > 0, context: c, relayAt: a.cfg.Guide.RelayAt}
	problems, err := a.store.Follow("guide-problems", func(e state.Event) bool { return e.Verb == "supervisor.file" })
	if err != nil {
		return nil, err
	}
	closed, err := a.closedNotes(st)
	if err != nil {
		return nil, err
	}
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
		dl, de := guideRole.fireRelayDue(st, q, a.guideDues, a.now)
		pl, pf := guideProblemLines(st, problems)
		fl, fed := a.feedLines(st, sessions, closed, findOrphaned(st, archived))
		lines = append(lines, pl...)
		fed = fed || pf
		lines = append(append(append(lines, rl...), dl...), fl...)
		evs := append(append(ce, re...), de...)
		return lines, evs, seen || fed || len(lines) > 0 || len(evs) > 0
	}
	if _, _, changed := fire(st); !changed {
		return nil, ctx.Err()
	}
	var lines []string
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		var evs []state.Event
		lines, evs, _ = fire(st)
		return evs, nil
	})
	return lines, err
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
	evs, err := a.store.Events(0, func(e state.Event) bool {
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
			if strings.HasPrefix(k, "problem:") {
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
	current := map[string]bool{}
	for _, e := range events {
		key, text, ok := keyed(e.Detail)
		// Legacy file events summarized a note already delivered to the guide;
		// only the format carrying the actual report belongs in this feed.
		if ok && strings.Contains(text, fileReportSeparator) {
			current["problem:"+key] = true
		}
	}
	guideRole.update(st, func(r *state.Role) {
		before := len(r.Fed)
		r.Fed = slices.DeleteFunc(r.Fed, func(key string) bool {
			return strings.HasPrefix(key, "problem:") && !current[key]
		})
		changed = len(r.Fed) != before
		for _, e := range events {
			key, text, ok := keyed(e.Detail)
			if !ok || !strings.Contains(text, fileReportSeparator) || !current["problem:"+key] || slices.Contains(r.Fed, "problem:"+key) {
				continue
			}
			r.Fed = append(r.Fed, "problem:"+key)
			lines = append(lines, "GUIDE PROBLEM: "+escapedLine(text))
			changed = true
		}
	})
	return lines, changed
}
