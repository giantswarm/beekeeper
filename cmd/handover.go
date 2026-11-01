package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/post"
	"github.com/giantswarm/beekeeper/internal/state"
)

// handoverSection is one section of the hand-over: print shows what a
// successor acts on, all (handover --section) everything of it.
type handoverSection struct {
	key, title string
	print      func(all bool)
}

func (a *app) handoverCmd() *cobra.Command {
	var events int
	var full bool
	var only string
	c := &cobra.Command{
		Use:   "handover",
		Short: "Everything on the desk, from the live state",
		Long: `Print the hand-over as Markdown: whether the supervisor loop runs,
the running sessions and what each is on, leases and grant queues, holds,
the merge lanes, registered agents, session records, the priorities and
holds, the board's epics in planning and past their go with their plans
(each plan's URL, state and head), the standing instructions (standing.md in the state directory, in
full),
open notes with their defaults, the decisions answered
within 72 hours, timers, what the alert watch reads and the latest
events. Everything comes from the state and the machine, so a reader acts
on it instead of a prose brief.

It shows what a reader acts on first: the notes the
guide serves (those for guide.person and for the guide) are one line, the
records of sessions ended over an hour ago are one line (an agent the roster
keeps, parked for one, stays listed), and the answered
decisions are those of the last day, with a count of the others answered
within 72 hours.
--section <name> prints one section with everything it holds: ` + "`" + `--section
notes` + "`" + ` every open note, ` + "`" + `--section records` + "`" + ` every record, ` + "`" + `--section
answers` + "`" + ` every decision answered within 72 hours.

The priorities and holds are read from GitHub with the read board next
makes: the priority list (the board's Priority field) by rank, each epic
with its Status, its plan line and its hold, then every held board item
with what holds it: its open blockers (board block), its Status set after
they closed, or the open decision that holds it (note add --holds) with its
person. A failed read is shown as not read, never filled from the state.

A caller that has read the hand-over before gets only what changed since:
the lines that are new or changed, the keys of those gone, or one "no
change" line. --full prints everything; --section and --json are always
complete.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v, err := a.collect(true)
			if err != nil {
				return err
			}
			l, err := a.leases()
			if err != nil {
				return err
			}
			evs, err := a.store.Events(events, func(e state.Event) bool { return !isRun(e) })
			if err != nil {
				return err
			}
			ans, err := a.handoverAnswers()
			if err != nil {
				return err
			}
			agents := a.agentViews(v.st, v.raw)
			holds := a.activeHolds(v.st)
			al, err := a.alertsHandover(cmd.Context())
			if err != nil {
				return err
			}
			pv := a.prioritiesHandover(cmd.Context(), v.st.Notes)
			ep := pv.epicsView()
			if pv != nil && pv.Error != "" {
				ep = a.epicsHandover(cmd.Context()) // the epics need no board.order
			}
			if a.json {
				return a.printJSON(struct {
					*view
					Leases     *leaseList      `json:"leases"`
					Holds      []state.Hold    `json:"holds"`
					Lanes      []laneView      `json:"lanes"`
					Agents     []agentView     `json:"agents"`
					Records    []state.Record  `json:"records"`
					Priorities *prioritiesView `json:"priorities,omitempty"`
					Epics      *epicsView      `json:"epics,omitempty"`
					Notes      []state.Note    `json:"notes"`
					Answered   []answered      `json:"answered"`
					Timers     []state.Timer   `json:"timers"`
					Alerts     *alertsView     `json:"alerts"`
					Events     []state.Event   `json:"events"`
				}{v, l, holds, a.laneViews(v.st), agents, v.st.Records, pv, ep, v.st.Notes, ans.All, v.st.Timers, al, evs})
			}
			lanes := a.laneViews(v.st)
			notes := a.splitNotes(v.st.Notes)
			standing, err := a.readStanding()
			if err != nil {
				return err
			}
			sections := []handoverSection{
				{"supervisor", "", func(bool) { a.printSupervisor(v) }},
				{secSessions, fmt.Sprintf("Sessions (%d running)", len(v.Sessions)), func(bool) { a.printSessions(v) }},
				{"leases", "Leases", func(bool) { a.printLeases(l) }},
				{"holds", "Holds", func(bool) { a.printHolds(holds) }},
				{"lanes", "Merge lanes", func(bool) { a.printLanes(lanes) }},
				{"agents", "Agents", func(bool) { a.printAgents(agents) }},
				{"records", "Session records", func(all bool) {
					if all {
						a.printRecords(v.st, v.st.Records, v.raw)
						return
					}
					a.printLiveRecords(v.st, v.st.Records, v.raw)
				}},
				{"priorities", "Priorities and holds", func(bool) { a.printPrioritiesHandover(pv) }},
				{"epics", "Epics", func(bool) { a.printEpicsHandover(ep) }},
				{"standing", "Standing instructions", func(bool) { a.printStanding(standing) }},
				{"notes", "Open notes", func(all bool) {
					if all {
						a.printNotes(v.st.Notes)
						return
					}
					a.printOwnNotes(notes)
				}},
				{"answers", "Answers", func(all bool) { a.printAnswers(ans, all) }},
				{"timers", "Timers", func(bool) { a.printTimers(v.st.Timers, false) }},
				{"alerts", "Alerts", func(bool) { a.printAlerts(al) }},
				{"events", "Latest events", func(bool) {
					for _, e := range evs {
						_, _ = fmt.Fprintf(a.out, "- %s\n", eventText(a, e))
					}
				}},
			}
			if only != "" {
				i := slices.IndexFunc(sections, func(s handoverSection) bool { return s.key == only })
				if i < 0 {
					return usageErr("--section %q is none of %s", only, sectionKeys(sections))
				}
				sections[i].print(true)
				return nil
			}
			printFull := func() {
				_, _ = fmt.Fprintf(a.out, "# Hand-over, %s (%s UTC)\n\n", a.now.Format("2006-01-02 15:04 MST"), a.now.UTC().Format("15:04"))
				for _, s := range sections {
					if s.title != "" {
						_, _ = fmt.Fprintf(a.out, "\n## %s\n\n", s.title)
					}
					s.print(false)
				}
			}
			var facts []fact
			for _, s := range sections {
				switch s.key {
				case secSessions:
					facts = append(facts, a.sessionFacts(v)...)
				case "agents":
					facts = append(facts, a.agentFacts(agents)...)
				case "events":
					for _, e := range evs {
						l := "event " + eventText(a, e)
						facts = append(facts, fact{Key: l, Sig: l, Line: l})
					}
				default:
					facts = append(facts, textFacts(s.key, a.capture(func() { s.print(false) }))...)
				}
			}
			return a.delta("handover", full, facts, printFull)
		},
	}
	fullFlag(c, &full)
	c.Flags().IntVar(&events, "events", 20, "how many of the latest events to include")
	c.Flags().StringVar(&only, "section", "", "print one section with everything it holds: "+handoverSections)
	return c
}

// handoverSections names the sections --section takes, in their order.
const handoverSections = "supervisor, sessions, leases, holds, lanes, agents, records, priorities, epics, standing, notes, answers, timers, alerts, events"

// epicsView is the board's epics in planning and past their go with their
// plans, or why they were not read; nil without a board.
type epicsView struct {
	Epics []board.Epic `json:"epics"`
	Error string       `json:"error,omitempty"`
}

// epicsHandover reads the board's epics for the hand-over; a failed read is
// its error, not the hand-over's.
func (a *app) epicsHandover(ctx context.Context) *epicsView {
	if a.cfg.Board.Owner == "" || a.cfg.Board.Project == 0 {
		return nil
	}
	epics, err := a.boardClient().Epics(ctx)
	if err != nil {
		return &epicsView{Error: err.Error()}
	}
	return &epicsView{Epics: epics}
}

// printEpicsHandover lists the epics with their plans.
func (a *app) printEpicsHandover(ep *epicsView) {
	switch {
	case ep == nil:
		_, _ = fmt.Fprintln(a.out, "No board configured.")
	case ep.Error != "":
		_, _ = fmt.Fprintf(a.out, "Not read: %s\n", ep.Error)
	case len(ep.Epics) == 0:
		_, _ = fmt.Fprintln(a.out, "No epic in planning or past its go.")
	default:
		a.printEpics(ep.Epics, "- ")
	}
}

// sectionKeys names the sections --section takes.
func sectionKeys(sections []handoverSection) string {
	keys := make([]string, len(sections))
	for i, s := range sections {
		keys[i] = s.key
	}
	return strings.Join(keys, ", ")
}

// printSupervisor says whether the supervisor loop runs.
func (a *app) printSupervisor(v *view) {
	if v.SupervisorLoop > 0 {
		_, _ = fmt.Fprintf(a.out, "Supervisor: the supervisor loop runs (PID %d).\n", v.SupervisorLoop)
		return
	}
	_, _ = fmt.Fprintf(a.out, "Supervisor: the supervisor loop does not run (%s).\n", loopServiceHint())
}

// eventText is one event of the log in a line.
func eventText(a *app, e state.Event) string {
	return fmt.Sprintf("%s %s (%s): %s", clock(a.now, e.At), e.Verb, truncate(e.By.Name, 30), truncate(strings.TrimSpace(e.Detail), 100))
}

// isRun is a build's run.start or run.end: machine traffic, which a
// successor's handover leaves out.
func isRun(e state.Event) bool { return strings.HasPrefix(e.Verb, "run.") }

func (a *app) logCmd() *cobra.Command {
	var n int
	var verb, since, until string
	c := &cobra.Command{
		Use:   "log",
		Short: "The event log: every claim, grant, hold, registration, note and build run",
		Long: `The event log: every claim, grant, hold, registration, note and build run.

Retention: events.jsonl in the state directory keeps every event since the
machine's first, never trimmed or rotated, so --since 30d answers the last 30
days in full. An agents.* event (start, wake, handover, note, broadcast) waits
for the state lock rather than give up on a busy one.

--since and --until bound the window (--until: before then); a bare date
(2026-10-08) is that day's 00:00 local time. Without either, -n shows the
latest 50 events; with one, every event in the window unless -n is given too.`,
		Example: `  beekeeper log --verb agents.problem --since 19:06
  beekeeper log --since 2026-10-08 --until 2026-10-09 --json   # the whole of October 8`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var from, to time.Time
			ref := a.now
			if until != "" {
				t, err := reportTime(a.now, until, time.Local)
				if err != nil {
					return err
				}
				to, ref = t, t
			}
			if since != "" {
				t, err := reportTime(ref, since, time.Local)
				if err != nil {
					return err
				}
				from = t
			}
			if !to.IsZero() && !from.Before(to) {
				return usageErr("--since %s is not before --until %s", from.Format(time.RFC3339), to.Format(time.RFC3339))
			}
			if (since != "" || until != "") && !cmd.Flags().Changed("lines") {
				n = 0
			}
			evs, err := a.store.Events(n, func(e state.Event) bool {
				return strings.HasPrefix(e.Verb, verb) && !e.At.Before(from) && (to.IsZero() || e.At.Before(to))
			})
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(evs)
			}
			w := a.table()
			for _, e := range evs {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", clock(a.now, e.At), e.Verb, truncate(e.By.Name, 30), truncate(e.Detail, 100))
			}
			return w.Flush()
		},
	}
	c.Flags().IntVarP(&n, "lines", "n", 50, "how many of the latest events (0: all; default all with --since or --until)")
	c.Flags().StringVar(&verb, "verb", "", "only the events whose verb starts with this (run.: the build runs)")
	c.Flags().StringVar(&since, "since", "", "only the events from this on: a duration before --until (1h, 30d), a time (15:04), a date (2006-01-02: its 00:00) or RFC 3339")
	c.Flags().StringVar(&until, "until", "", "only the events before this: a time (15:04), a date (2006-01-02: its 00:00, the day itself left out) or RFC 3339; default now")
	c.AddCommand(&cobra.Command{
		Use:   "add <text>",
		Short: "Log a status line: what happened, for whoever reads the log (verb status)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			text := strings.Join(args, " ")
			if a.managedExecutionID() != "" {
				return a.remoteControl(cmd.Context(), "event", executionEvent{Text: text})
			}
			if m := post.Unlinked(text, ""); len(m) > 0 {
				return refused("log line refused, it lacks: %s", strings.Join(m, "; "))
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			return a.store.Update(func(*state.State) ([]state.Event, error) {
				return []state.Event{event(me, "status", "%s", text)}, nil
			})
		},
	})
	return c
}
