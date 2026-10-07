package cmd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

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
	var prompt, full bool
	var only string
	c := &cobra.Command{
		Use:   "handover",
		Short: "Everything the next supervisor needs, from the live state",
		Long: `Print the hand-over as Markdown: the supervisor and its context in tokens,
the running sessions and what each is on, leases and grant queues, holds,
the merge lanes, registered agents, session records, pinned notes, open
notes with their defaults, the decisions answered since the last relay,
timers, what the alert watch reads and the latest events. Everything comes
from the state and the machine, so a successor (or the same supervisor
after a restart) reads it instead of a prose brief.

It shows what a successor acts on in its first minutes: the notes the
guide serves (those for guide.person and for the guide) are one line, the
records of sessions ended over an hour ago are one line, and the answered
decisions are those since the predecessor's start, with a count of the
others answered within 72 hours. Pinned notes, the standing instructions
(note add --pin), are in every hand-over until unpinned.
--section <name> prints one section with everything it holds: ` + "`" + `--section
notes` + "`" + ` every open note, ` + "`" + `--section records` + "`" + ` every record, ` + "`" + `--section
answers` + "`" + ` every decision answered within 72 hours.

--prompt prints the successor's session prompt instead: the configured
instructions (supervisor.skill or supervisor.instructions), the scope
(supervisor.scope), the pending state as selected above and the commands
that read the live values. It carries no live value: no version, memory
figure or pull request state.

A caller that has read the hand-over before gets only what changed since:
the lines that are new or changed, the keys of those gone, or one "no
change" line. --full prints everything; --prompt, --section and --json are
always complete.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v, err := a.collect(!prompt) // the prompt names no session's current work
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
			ans, err := a.handoverAnswers(v.st)
			if err != nil {
				return err
			}
			agents := a.agentViews(v.st, v.raw)
			holds := a.activeHolds(v.st)
			al, err := a.alertsHandover(cmd.Context())
			if err != nil {
				return err
			}
			if prompt {
				return a.printPrompt(cmd.Context(), v, l, al, ans)
			}
			if a.json {
				return a.printJSON(struct {
					*view
					Leases   *leaseList     `json:"leases"`
					Holds    []state.Hold   `json:"holds"`
					Lanes    []laneView     `json:"lanes"`
					Agents   []agentView    `json:"agents"`
					Records  []state.Record `json:"records"`
					Notes    []state.Note   `json:"notes"`
					Answered []answered     `json:"answered"`
					Timers   []state.Timer  `json:"timers"`
					Alerts   *alertsView    `json:"alerts"`
					Events   []state.Event  `json:"events"`
				}{v, l, holds, a.laneViews(v.st), agents, v.st.Records, v.st.Notes, ans.All, v.st.Timers, al, evs})
			}
			lanes := a.laneViews(v.st)
			notes := a.splitNotes(v.st.Notes)
			sections := []handoverSection{
				{"supervisor", "", func(bool) { a.printSupervisor(v) }},
				{secSessions, fmt.Sprintf("Sessions (%d running)", len(v.Sessions)), func(bool) { a.printSessions(v) }},
				{"leases", "Leases", func(bool) { a.printLeases(l) }},
				{"holds", "Holds", func(bool) { a.printHolds(holds) }},
				{"lanes", "Merge lanes", func(bool) { a.printLanes(lanes) }},
				{"agents", "Agents", func(bool) { a.printAgents(agents) }},
				{"records", "Session records", func(all bool) {
					if all {
						a.printRecords(v.st.Records, v.raw)
						return
					}
					a.printLiveRecords(v.st.Records, v.raw)
				}},
				{"pinned", "Pinned notes", func(bool) { a.printPinned(notes.Pinned) }},
				{"notes", "Open notes", func(all bool) {
					if all {
						a.printNotes(v.st.Notes)
						return
					}
					a.printOwnNotes(notes)
				}},
				{"answers", "Answered decisions", func(all bool) { a.printAnswers(ans, all) }},
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
	c.Flags().BoolVar(&prompt, "prompt", false, "print the successor's session prompt: instructions, scope and the pending state")
	c.Flags().StringVar(&only, "section", "", "print one section with everything it holds: "+handoverSections)
	return c
}

// handoverSections names the sections --section takes, in their order.
const handoverSections = "supervisor, sessions, leases, holds, lanes, agents, records, pinned, notes, answers, timers, alerts, events"

// sectionKeys names the sections --section takes.
func sectionKeys(sections []handoverSection) string {
	keys := make([]string, len(sections))
	for i, s := range sections {
		keys[i] = s.key
	}
	return strings.Join(keys, ", ")
}

// printSupervisor says who supervises and an open relay.
func (a *app) printSupervisor(v *view) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
	switch {
	case v.Supervisor == nil:
		p("No supervisor is recorded.")
	case v.Supervisor.Live:
		p("Supervisor: %q since %s%s.", v.Supervisor.Name, clock(a.now, v.Supervisor.Since), v.Supervisor.contextText())
	default:
		p("Supervisor: %q since %s, its session is gone.", v.Supervisor.Name, clock(a.now, v.Supervisor.Since))
	}
	if r := v.st.Relay; r.Open(a.now) {
		p("Relayed to %q until %s: its `beekeeper supervisor start` takes the role.", r.To.Name, clock(a.now, r.Expires))
	}
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
	var verb string
	c := &cobra.Command{
		Use:   "log",
		Short: "The event log: every claim, grant, hold, registration, note and build run",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			evs, err := a.store.Events(n, func(e state.Event) bool { return strings.HasPrefix(e.Verb, verb) })
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
	c.Flags().IntVarP(&n, "lines", "n", 50, "how many of the latest events (0: all)")
	c.Flags().StringVar(&verb, "verb", "", "only the events whose verb starts with this (run.: the build runs)")
	c.AddCommand(&cobra.Command{
		Use:   "add <text>",
		Short: "Log a status line: what happened, for whoever reads the log (verb status)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			return a.store.Update(func(*state.State) ([]state.Event, error) {
				return []state.Event{event(me, "status", "%s", strings.Join(args, " "))}, nil
			})
		},
	})
	return c
}
