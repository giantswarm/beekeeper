package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) handoverCmd() *cobra.Command {
	var events int
	c := &cobra.Command{
		Use:   "handover",
		Short: "Everything the next supervisor needs, from the live state",
		Long: `Print the hand-over as Markdown: the supervisor, the running sessions and
what each is on, overlaps, leases and grant queues, holds, registered
agents, open notes and the latest events. Everything comes from the live
state and the machine, so a successor (or the same supervisor after a
restart) reads it instead of a prose brief.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			v, err := a.collect(true)
			if err != nil {
				return err
			}
			l, err := a.leases()
			if err != nil {
				return err
			}
			evs, err := a.store.Events(events)
			if err != nil {
				return err
			}
			agents := a.agentViews(v.st, v.raw)
			holds := a.activeHolds(v.st)
			if a.json {
				return a.printJSON(struct {
					*view
					Leases *leaseList    `json:"leases"`
					Holds  []state.Hold  `json:"holds"`
					Lanes  []laneView    `json:"lanes"`
					Agents []agentView   `json:"agents"`
					Notes  []state.Note  `json:"notes"`
					Events []state.Event `json:"events"`
				}{v, l, holds, a.laneViews(v.st), agents, v.st.Notes, evs})
			}
			p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
			p("# Hand-over, %s (%s UTC)\n", a.now.Format("2006-01-02 15:04 MST"), a.now.UTC().Format("15:04"))
			switch {
			case v.Supervisor == nil:
				p("No supervisor is recorded.")
			case v.Supervisor.Live:
				p("Supervisor: %q since %s.", v.Supervisor.Name, clock(a.now, v.Supervisor.Since))
			default:
				p("Supervisor: %q since %s, its session is gone.", v.Supervisor.Name, clock(a.now, v.Supervisor.Since))
			}
			p("\n## Sessions (%d running)\n", len(v.Sessions))
			a.printSessions(v)
			p("\n## Leases\n")
			a.printLeases(l)
			p("\n## Holds\n")
			a.printHolds(holds)
			p("\n## Merge lanes\n")
			a.printLanes(a.laneViews(v.st))
			p("\n## Agents\n")
			a.printAgents(agents)
			p("\n## Open notes\n")
			a.printNotes(v.st.Notes)
			if len(evs) > 0 {
				p("\n## Latest events\n")
				for _, e := range evs {
					p("- %s %s (%s): %s", clock(a.now, e.At), e.Verb, truncate(e.By.Name, 30), truncate(strings.TrimSpace(e.Detail), 100))
				}
			}
			return nil
		},
	}
	c.Flags().IntVar(&events, "events", 20, "how many of the latest events to include")
	return c
}

func (a *app) logCmd() *cobra.Command {
	var n int
	c := &cobra.Command{
		Use:   "log",
		Short: "The event log: every claim, grant, hold, registration and note",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			evs, err := a.store.Events(n)
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
	return c
}
