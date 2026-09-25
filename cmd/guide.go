package cmd

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/signal"
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
It is never the supervisor's session: ` + "`guide start`" + ` in the supervisor's
session and ` + "`supervisor start`" + ` in the guide's are refused. It has no grant
power; the grant rule stays the supervisor's.

The role moves like the supervisor's, in two steps that leave no gap: the
guide names its successor (relay), the successor starts
(` + "`beekeeper guide handover --prompt`" + ` is its prompt). ` + "`beekeeper guide watch`" + `
says GUIDE RELAY DUE once the guide's context reaches guide.relayAt.

Its queue is every open note filed --for a person (` + "`beekeeper guide queue`" + `)
and every session the desktop files as waiting on its person;
` + "`beekeeper note answer <id> <answer>`" + ` records an answer word for word and closes
the note.

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
		RunE:  func(*cobra.Command, []string) error { return a.runStart(guideRole, takeOver) },
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
	c.AddCommand(start, a.relayCmd(guideRole, guideRelayLong), stop, status, a.guideQueueCmd(), a.guideHandoverCmd(), a.guideWatchCmd())
	return c
}

const guideRelayLong = `Name the session that takes over the guide's role. Its
` + "`beekeeper guide start`" + ` takes it; until then you stay the guide. The relay
stays open for guide.relayTTL (default 15m), then expires and you simply
keep guiding; --cancel withdraws it earlier. The supervisor's role is not
touched. Once the successor has started, ` + "`beekeeper guide status`" + ` in your
session exits 4. The successor is a name, a unique part of one, a session
id or a PID.`

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

// guideQueue is the guide's queue: the open --for notes in filing order,
// then the sessions waiting on their person.
func guideQueue(st *state.State, sessions []*claude.Session) []queueItem {
	var out []queueItem
	for i := range st.Notes {
		n := &st.Notes[i]
		if n.For == "" {
			continue
		}
		_, live := claude.Live(sessions, n.By)
		out = append(out, queueItem{Note: n, Owner: n.By.Name, OwnerLive: live, Key: "note#" + strconv.Itoa(n.ID)})
	}
	for _, s := range sessions {
		if s.Waiting != nil {
			out = append(out, queueItem{Owner: s.Name, OwnerLive: true, Waiting: s.Waiting.Action,
				Key: "wait:" + cmp.Or(s.HostID, s.ID) + ":" + s.Waiting.Turn})
		}
	}
	return out
}

// text is the item in one line: owner, status quo, deadline and default.
func (a *app) queueText(q queueItem) string {
	if q.Note == nil {
		return fmt.Sprintf("%q waits on its person: %s", q.Owner, oneLine(q.Waiting))
	}
	n := q.Note
	owner := fmt.Sprintf("from %q", q.Owner)
	if !q.OwnerLive && (n.By.Session != "" || n.By.HostSession != "") { // a person or script does not end
		owner += " (ended)"
	}
	s := fmt.Sprintf("#%d for %s %s", n.ID, n.For, owner)
	if !n.Due.IsZero() {
		s += ", due " + clock(a.now, n.Due)
		if !n.Due.After(a.now) {
			s += " (overdue)"
		}
	}
	s += ": " + oneLine(n.Text)
	if n.Default != "" {
		s += "; if unanswered: " + oneLine(n.Default)
	}
	return s
}

func (a *app) guideQueueCmd() *cobra.Command {
	var full bool
	c := &cobra.Command{
		Use:   "queue",
		Short: "The decisions waiting on the person: open --for notes and waiting sessions, with their owners",
		Long: `Every open note filed --for a person, with the session that filed it
(its owner, and whether it still runs), its deadline and its default, then
every session the desktop files as waiting on its person with what it
needs. A caller that has read the queue before gets only what changed.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			q := guideQueue(st, sessions)
			if a.json {
				return a.printJSON(q)
			}
			return a.delta("guide-queue", full, a.queueFacts(q), func() { a.printQueue(q) })
		},
	}
	fullFlag(c, &full)
	return c
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
the configured instructions (guide.skill, default guide, or
guide.instructions), the queue, an open relay and the commands that read
the live values; no standing rule and no live value. A caller that has read
the hand-over before gets only what changed; --full prints everything.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			q := guideQueue(st, sessions)
			r := guideRole.get(st)
			if prompt {
				return a.printGuidePrompt(r, q)
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
			return a.delta("guide-handover", full, facts, func() {
				_, _ = fmt.Fprintf(a.out, "# Guide hand-over, %s\n\n%s\n", a.now.Format("2006-01-02 15:04 MST"), head)
				if relay != "" {
					_, _ = fmt.Fprintln(a.out, relay)
				}
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
	{"beekeeper guide queue", "the decisions waiting on the person now"},
	{"beekeeper guide status", "the guide and its context against guide.relayAt"},
	{"beekeeper log --verb note.", "the notes filed, answered and closed"},
}

func (a *app) printGuidePrompt(r state.Role, q []queueItem) error {
	intro, err := a.roleInstructions(guideRole, r)
	if err != nil {
		return err
	}
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
	p("%s\n", intro)
	p("## Pending state, %s\n", a.stamp(a.now))
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
		Long: `One line, once, for each open note newly filed --for a person, each session
the desktop newly files as waiting on its person (read from its session
record, never from a transcript), each note of the queue answered or
closed, and the guide's relay: GUIDE RELAY DUE at guide.relayAt, taken or
expired, and a restart of its CLI. Silent otherwise. What it said is kept
in the state, so a restarted feed says nothing again. Runs until killed;
--once polls once.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			tick := time.NewTicker(a.cfg.Watch.Interval.Duration)
			defer tick.Stop()
			for {
				a.now = time.Now()
				lines, err := a.guideFeed(ctx)
				if err != nil {
					lines = []string{fmt.Sprintf("GUIDE FEED: %v", err)}
				}
				for _, l := range lines {
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
	c := roleContext(guideRole.get(st), sessions, a.now, a.cfg.Guide.RelayAt)
	q := quietness{checked: c > 0, context: c}
	closed, err := a.closedNotes(st)
	if err != nil {
		return nil, err
	}
	fire := func(st *state.State) ([]string, []state.Event, bool) {
		seen, ce := guideRole.observeCLI(st, sessions, a.now)
		var lines []string
		for _, e := range ce {
			lines = append(lines, fmt.Sprintf("GUIDE RESTARTED: %q, %s; it keeps the role", e.By.Name, e.Detail))
		}
		rl, re := guideRole.fireRelay(st, a.now)
		dl, de := guideRole.fireRelayDue(st, q, a.now)
		fl, fed := a.feedLines(st, sessions, closed)
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
// that are no longer open, by note id: their answer, or their done.
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
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "note.answered" || e.Verb == "note.done" })
	if err != nil {
		return nil, err
	}
	for _, e := range evs {
		id, _, _ := strings.Cut(strings.TrimPrefix(e.Detail, "#"), " ")
		if n, err := strconv.Atoi(id); err == nil && gone[n] {
			out[n] = e // the latest wins
		}
	}
	return out, nil
}

// feedLines says each queue item the feed has not said yet and each note it
// said that is closed now, and records what it said in the guide's Fed. It
// reports whether Fed changed.
func (a *app) feedLines(st *state.State, sessions []*claude.Session, closed map[int]state.Event) (lines []string, changed bool) {
	guideRole.update(st, func(r *state.Role) {
		var cur []string
		for _, it := range guideQueue(st, sessions) {
			k := it.Key
			cur = append(cur, k)
			if slices.Contains(r.Fed, k) {
				continue
			}
			if it.Note != nil {
				lines = append(lines, "GUIDE DECISION: "+truncate(a.queueText(it), 240))
			} else {
				lines = append(lines, fmt.Sprintf("GUIDE WAITING: %q needs its person: %s", it.Owner, truncate(oneLine(it.Waiting), 200)))
			}
		}
		for _, k := range r.Fed {
			id, err := strconv.Atoi(strings.TrimPrefix(k, "note#"))
			if err != nil || slices.Contains(cur, k) {
				continue
			}
			switch e, ok := closed[id]; {
			case ok && e.Verb == "note.answered":
				lines = append(lines, fmt.Sprintf("GUIDE ANSWERED (%s): %s", truncate(e.By.Name, 30), truncate(oneLine(e.Detail), 240)))
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
