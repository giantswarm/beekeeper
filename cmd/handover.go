package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) handoverCmd() *cobra.Command {
	var events int
	var prompt, full bool
	c := &cobra.Command{
		Use:   "handover",
		Short: "Everything the next supervisor needs, from the live state",
		Long: `Print the hand-over as Markdown: the supervisor and its context in tokens,
the running sessions and what each is on, overlaps, leases and grant
queues, holds, the merge lanes, registered agents, session records, open
notes with their defaults, timers, what the alert watch reads and the
latest events. Everything comes from the
state and the machine, so a successor (or the same supervisor after a
restart) reads it instead of a prose brief.

--prompt prints the successor's session prompt instead: the configured
instructions (supervisor.skill or supervisor.instructions), the scope
(supervisor.scope), the pending state in full and the commands that read
the live values. It carries no standing rule and no live value: no version,
memory figure or pull request state.

A caller that has read the hand-over before gets only what changed since:
the lines that are new or changed, the keys of those gone, or one "no
change" line. --full prints everything; --prompt and --json are always
complete.`,
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
			agents := a.agentViews(v.st, v.raw)
			holds := a.activeHolds(v.st)
			al, err := a.alertsHandover(cmd.Context())
			if err != nil {
				return err
			}
			if prompt {
				return a.printPrompt(cmd.Context(), v, l, al)
			}
			if a.json {
				return a.printJSON(struct {
					*view
					Leases  *leaseList     `json:"leases"`
					Holds   []state.Hold   `json:"holds"`
					Lanes   []laneView     `json:"lanes"`
					Agents  []agentView    `json:"agents"`
					Records []state.Record `json:"records"`
					Notes   []state.Note   `json:"notes"`
					Timers  []state.Timer  `json:"timers"`
					Alerts  *alertsView    `json:"alerts"`
					Events  []state.Event  `json:"events"`
				}{v, l, holds, a.laneViews(v.st), agents, v.st.Records, v.st.Notes, v.st.Timers, al, evs})
			}
			lanes := a.laneViews(v.st)
			sup := a.capture(func() { a.printSupervisor(v) })
			printFull := func() {
				p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
				p("# Hand-over, %s (%s UTC)\n", a.now.Format("2006-01-02 15:04 MST"), a.now.UTC().Format("15:04"))
				_, _ = fmt.Fprint(a.out, sup)
				p("\n## Sessions (%d running)\n", len(v.Sessions))
				a.printSessions(v)
				p("\n## Leases\n")
				a.printLeases(l)
				p("\n## Holds\n")
				a.printHolds(holds)
				p("\n## Merge lanes\n")
				a.printLanes(lanes)
				p("\n## Agents\n")
				a.printAgents(agents)
				p("\n## Session records\n")
				a.printRecords(v.st.Records, v.raw)
				p("\n## Open notes\n")
				a.printNotes(v.st.Notes)
				p("\n## Timers\n")
				a.printTimers(v.st.Timers)
				p("\n## Alerts\n")
				a.printAlerts(al)
				if len(evs) > 0 {
					p("\n## Latest events\n")
					for _, e := range evs {
						p("- %s", eventText(a, e))
					}
				}
			}
			facts := textFacts("supervisor", sup)
			facts = append(facts, a.sessionFacts(v)...)
			for section, text := range map[string]string{
				"lease": a.capture(func() { a.printLeases(l) }),
				"hold":  a.capture(func() { a.printHolds(holds) }),
				"lane":  a.capture(func() { a.printLanes(lanes) }),
				"note":  a.capture(func() { a.printNotes(v.st.Notes) }),
				"timer": a.capture(func() { a.printTimers(v.st.Timers) }),
				"alert": a.capture(func() { a.printAlerts(al) }),
			} {
				facts = append(facts, textFacts(section, text)...)
			}
			facts = append(facts, a.agentFacts(agents)...)
			for _, e := range evs {
				l := "event " + eventText(a, e)
				facts = append(facts, fact{Key: l, Sig: l, Line: l})
			}
			return a.delta("handover", full, facts, printFull)
		},
	}
	fullFlag(c, &full)
	c.Flags().IntVar(&events, "events", 20, "how many of the latest events to include")
	c.Flags().BoolVar(&prompt, "prompt", false, "print the successor's session prompt: instructions, scope and the pending state")
	return c
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
	if sp := v.st.Spare; sp != nil {
		p("Spare: %q, kept awake by the standby watch; it takes the role after a crash.", sp.Name)
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
	return c
}
