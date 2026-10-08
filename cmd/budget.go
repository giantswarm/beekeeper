package cmd

import (
	"fmt"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

type poller struct {
	PID     int           `json:"pid"`
	Session string        `json:"session,omitempty"`
	Args    string        `json:"args"`
	Elapsed time.Duration `json:"elapsed"`
}

func (a *app) budgetCmd() *cobra.Command {
	var gate bool
	var floor int
	c := &cobra.Command{
		Use:   "budget",
		Short: "The GitHub REST and GraphQL budget every session draws from, and who is drawing",
		Long: `Read the person's GitHub core budget from the rate-limit headers of a real,
conditional request (a 304 costs nothing; /rate_limit is exempt and lies),
and the GraphQL limit from a real rateLimit query (one point, read again
after merge.budgetFresh), and list the gh and devctl processes on the
machine with the session each runs under: the callers drawing on it. A
GraphQL refusal shows even while its counter looks healthy (a secondary
limit), with the time it ends when GitHub names one. The budget is shared
with the person's own logins, the developer portal among them: at zero it
signs them out. The urgent merge of the reset window (lanes urgent), the
one that ran under the floor or the one marked and waiting, is a line with
who asked for it and what its run drew against its own bound.

--gate exits 3 when GitHub refuses GraphQL calls, the budget is under the
floor (github.floor, default 2500) or a "github" hold is set:
` + "`beekeeper budget --gate && gh …`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if floor > 0 {
				a.cfg.GitHub.Floor = floor
			}
			b, err := a.probeBudget(cmd.Context())
			if err != nil {
				return err
			}
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			var pollers []poller
			sessions, t, callersErr := a.sessions()
			if callersErr == nil {
				pollers = githubCallers(a, t, sessions)
			}
			hold, held := activeHold(st, a, "github")
			urgent := urgentLines(a, st)
			if a.json {
				_ = a.printJSON(struct {
					github.Budget
					Floor   int            `json:"floor"`
					Held    bool           `json:"held"`
					Urgent  []state.Urgent `json:"urgent,omitempty"`
					Pollers []poller       `json:"pollers"`
				}{b, a.cfg.GitHub.Floor, held, st.Urgent, pollers})
			} else {
				_, _ = fmt.Fprintln(a.out, budgetLine(a, b))
				for _, l := range urgent {
					_, _ = fmt.Fprintln(a.out, l)
				}
				if held {
					_, _ = fmt.Fprintf(a.out, "GitHub is held by %q until %s: %s\n", hold.By.Name, untilText(a, hold), hold.Reason)
				}
				if callersErr != nil {
					_, _ = fmt.Fprintf(a.out, "callers unknown: %v\n", callersErr)
				}
				if len(pollers) > 0 {
					_, _ = fmt.Fprintf(a.out, "%d gh/devctl processes:\n", len(pollers))
					w := a.table()
					for _, p := range pollers {
						owner := p.Session
						if owner == "" {
							owner = noSession
						}
						_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\n", dur(p.Elapsed), truncate(p.Args, 70), truncate(owner, 40))
					}
					_ = w.Flush()
				}
			}
			if gate && held {
				return refused("GitHub is held: %s", hold.Reason)
			}
			if gate && b.GraphQL.Blocks(a.now) {
				return refused("GitHub refuses GraphQL calls: %s", graphqlText(a, b.GraphQL))
			}
			if gate && b.Remaining < a.cfg.GitHub.Floor {
				return refused("GitHub budget %d is under the floor %d: wait for the reset at %s", b.Remaining, a.cfg.GitHub.Floor, clock(a.now, b.Reset))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&gate, "gate", false, "exit 3 when GraphQL is refused, under the floor or held")
	c.Flags().IntVar(&floor, "floor", 0, "the floor for this call (default github.floor)")
	return c
}

// githubCallers are the gh and devctl processes drawing on the budget, with
// the session each runs under: longest-running first.
func githubCallers(a *app, t *proc.Table, sessions []*claude.Session) []poller {
	var out []poller
	for _, p := range t.ByPID {
		if p.Comm != "gh" && p.Comm != merge.Tool {
			continue
		}
		pl := poller{PID: p.PID, Args: p.Cmdline(), Elapsed: p.Elapsed(a.now).Round(time.Second)}
		if s, ok := claude.OwnerOf(sessions, p.PID); ok {
			pl.Session = s.Name
		}
		out = append(out, pl)
	}
	slices.SortFunc(out, func(x, y poller) int { return int(y.Elapsed - x.Elapsed) })
	return out
}
