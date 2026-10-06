package cmd

import (
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) agentArchivableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "archivable <local_ desktop id>...",
		Short: "Confirm that desktop sessions are finished workers beekeeper started, which a steward may archive",
		Long: `Confirms, for each desktop session named by its local_ id, that it is a
finished worker beekeeper started: one of beekeeper's starts, off the
roster, holding or having held no role a relay did not relieve, its desktop
record unarchived and its CLI in no turn (the calling session's own turn
aside). Those are the sessions agents.archiveAgreement covers, and a steward
asked to archive runs this first and archives only what it confirms.

A session the person started is never confirmed. Exits 3 when any named
session is not confirmed, with why.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			self, _ := a.callerSession()
			var refusedIDs []string
			for _, host := range args {
				name, why := a.archivableHost(st, host, self)
				if why != "" {
					refusedIDs = append(refusedIDs, host)
					if _, err := fmt.Fprintf(a.out, "%s: not archivable: %s\n", host, why); err != nil {
						return err
					}
					continue
				}
				if _, err := fmt.Fprintf(a.out, "%s: archivable: %q, a finished worker beekeeper started\n", host, name); err != nil {
					return err
				}
			}
			if len(refusedIDs) > 0 {
				return refused("%d of %d not archivable: archive only the confirmed ones", len(refusedIDs), len(args))
			}
			return nil
		},
	}
}

// archivableHost is the name of the finished worker whose desktop session
// host is, and why it is not archivable, "" when it is. The turn of self,
// the steward asking, does not count.
func (a *app) archivableHost(st *state.State, host string, self state.Party) (name, why string) {
	i := slices.IndexFunc(st.Starts, func(s state.Start) bool { return s.HostSession == host })
	if i < 0 {
		return "", "beekeeper did not start it (a session the person started is never archived)"
	}
	p := st.Starts[i].Party
	if slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) }) {
		return p.Name, "it is on the roster"
	}
	got, why := a.archivableRecord(st, p)
	if why != "" {
		return p.Name, why
	}
	if got != host {
		return p.Name, "its start names another desktop session"
	}
	if s, ok := a.runningTurn(p); ok && !p.Is(self) {
		return p.Name, fmt.Sprintf("its CLI %d is in a turn", s.PID)
	}
	return p.Name, ""
}
