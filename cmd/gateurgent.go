package cmd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) urgentCmd() *cobra.Command {
	var reason string
	c := &cobra.Command{
		Use:   "urgent <owner/repo> <n> --reason <why>",
		Short: "Let one merge run under the GitHub budget floor, once per reset window",
		Long: `urgent marks a merge urgent: a privacy or security fix whose delay keeps
something exposed, not a convenience. Its gate runs it while the GitHub budget
is under the floor (github.floor) instead of queueing it for the reset, as long
as the budget keeps the merge's own bound (github.urgentBound, 200); what its
run drew is logged against that bound. The mark is the supervisor's go for
that one merge: the gate's log (merge.urgent, merge.urgent.spent) and beekeeper
budget name the merge and who asked for it.

One urgent merge runs under the floor per reset window. A second mark while
one waits or after one ran in the window is refused with the reason (exit 3),
and so is a marked merge whose gate finds the window's run taken: it waits for
the reset like any other. A merge already queued for the reset (its run of its
own) picks the mark up at its next check; a mark whose merge has not reached
its gate is kept for merge.seedTTL. Above the floor a marked merge runs as any
other and its mark is dropped.`,
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			pr, err := prArgs(args)
			if err != nil {
				return err
			}
			if strings.TrimSpace(reason) == "" {
				return usageErr("--reason <why> is required: the fix it carries, logged with who asked")
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			u := state.Urgent{Repo: args[0], PR: pr, By: me, Reason: reason, At: a.now.UTC()}
			var why string
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				pruneUrgent(st, a.now, a.cfg.Merge.SeedTTL.Duration)
				if why = urgentTaken(a, st, u.Key()); why != "" {
					return []state.Event{event(me, "merge.urgent.refused", "%s: %s", u.Key(), why)}, nil
				}
				st.Urgent = slices.DeleteFunc(st.Urgent, func(o state.Urgent) bool { return o.Key() == u.Key() })
				st.Urgent = append(st.Urgent, u)
				return []state.Event{event(me, "merge.urgent.marked", "%s asked by %q: %s", u.Key(), me.Name, reason)}, nil
			})
			switch {
			case err != nil:
				return err
			case why != "":
				return refused("%s is not marked urgent: %s", u.Key(), why)
			}
			_, err = fmt.Fprintf(a.out, "marked %s urgent: its gate runs it under the budget floor while %d remain, once in this reset window\n",
				u.Key(), a.cfg.GitHub.UrgentBound)
			return err
		},
	}
	c.Flags().StringVar(&reason, "reason", "", "the fix the merge carries, logged with who asked")
	return c
}

// urgentLines are beekeeper budget's lines on the urgent merges: the one that
// ran under the floor in this reset window, or the one marked and waiting.
func urgentLines(a *app, st *state.State) []string {
	pruneUrgent(st, a.now, a.cfg.Merge.SeedTTL.Duration)
	var out []string
	for _, u := range st.Urgent {
		if !u.Ran() {
			out = append(out, fmt.Sprintf("urgent: %s waits for its gate, asked by %q at %s: %s", u.Key(), u.By.Name, clock(a.now, u.At), u.Reason))
			continue
		}
		drew := "it runs"
		switch {
		case u.Spent == nil:
		case *u.Spent < 0:
			drew = "drew an unread amount"
		default:
			drew = fmt.Sprintf("drew %d of its own bound %d", *u.Spent, a.cfg.GitHub.UrgentBound)
		}
		out = append(out, fmt.Sprintf("urgent: %s ran under the floor at budget %d, asked by %q: %s; %s; the next urgent merge after the reset at %s",
			u.Key(), u.Remaining, u.By.Name, u.Reason, drew, clock(a.now, u.Window)))
	}
	return out
}
