package cmd

import (
	"fmt"
	"slices"
	"strings"
	"time"

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

// pruneUrgent drops the urgent merges whose reset window ended and the marks
// whose merge has not reached its gate within ttl.
func pruneUrgent(st *state.State, now time.Time, ttl time.Duration) {
	st.Urgent = slices.DeleteFunc(st.Urgent, func(u state.Urgent) bool {
		if u.Ran() {
			return !now.Before(u.Window)
		}
		return now.Sub(u.At) > ttl
	})
}

// urgentTaken is why another merge than key cannot be urgent now: one ran
// under the floor in this reset window, or one is marked and waits; "" when
// none.
func urgentTaken(a *app, st *state.State, key string) string {
	for _, u := range st.Urgent {
		switch {
		case u.Key() == key:
		case u.Ran():
			return fmt.Sprintf("%s ran under the floor in this reset window, asked by %q (%s); one urgent merge per window, the next after the reset at %s",
				u.Key(), u.By.Name, u.Reason, clock(a.now, u.Window))
		default:
			return fmt.Sprintf("%s is marked urgent and waits for its gate, asked by %q at %s (%s); one urgent merge per window",
				u.Key(), u.By.Name, clock(a.now, u.At), u.Reason)
		}
	}
	return ""
}

// urgentTurn decides a merge under the budget floor: run says it runs now as
// the window's urgent merge (marked by lanes urgent); otherwise why is what
// it waits for when the mark keeps it waiting, "" for the plain floor wait. A
// mark that finds the window's run taken is refused and dropped.
func (g *gateRun) urgentTurn(b state.Budget) (run bool, why string, err error) {
	bound := g.cfg.GitHub.UrgentBound
	var refusal string
	err = g.store.Update(func(st *state.State) ([]state.Event, error) {
		pruneUrgent(st, g.now, g.cfg.Merge.SeedTTL.Duration)
		i := slices.IndexFunc(st.Urgent, func(u state.Urgent) bool { return u.Key() == g.key() })
		if i < 0 {
			return nil, nil
		}
		u := &st.Urgent[i]
		if refusal = urgentTaken(g.app, st, g.key()); refusal != "" {
			st.Urgent = slices.Delete(st.Urgent, i, i+1)
			return []state.Event{event(g.me, "merge.urgent.refused", "%s: %s; it waits for the reset", g.key(), refusal)}, nil
		}
		if b.Remaining < bound {
			why = fmt.Sprintf("%s is urgent (asked by %q), and the GitHub budget %d is under its own bound %d until the reset at %s",
				g.key(), u.By.Name, b.Remaining, bound, clock(g.now, b.Reset))
			return nil, nil
		}
		run = true
		u.Window, u.Remaining = b.Reset.UTC(), b.Remaining
		return []state.Event{event(g.me, "merge.urgent", "%s runs under the floor (budget %d, floor %d, its own bound %d) as the urgent merge of the window resetting at %s, asked by %q: %s",
			g.key(), b.Remaining, g.cfg.GitHub.Floor, bound, clock(g.now, b.Reset), u.By.Name, u.Reason)}, nil
	})
	if refusal != "" {
		gateLine("urgent refused, %s: %s waits for the reset like any other", refusal, g.key())
	}
	if run {
		gateLine("urgent: %s runs under the budget floor (%d of the floor %d, its own bound %d), once in this reset window", g.key(), b.Remaining, g.cfg.GitHub.Floor, bound)
	}
	return run, why, err
}

// settleUrgent ends the merge's urgent mark once devctl ran: an urgent run
// under the floor has what it drew from the budget logged against its bound
// (the window's record stays until the reset), a mark that was not needed
// above the floor is dropped.
func (g *gateRun) settleUrgent(urgent bool, before state.Budget) {
	spent := -1
	if urgent {
		g.now = time.Now()
		if after, err := g.gateBudget(); err == nil && after.Reset.Equal(before.Reset) {
			spent = before.Remaining - after.Remaining
		}
	}
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(st.Urgent, func(u state.Urgent) bool { return u.Key() == g.key() })
		switch {
		case i < 0:
			return nil, nil
		case !urgent || !st.Urgent[i].Ran():
			st.Urgent = slices.Delete(st.Urgent, i, i+1)
			return nil, nil
		}
		st.Urgent[i].Spent = &spent
		drew := "an unread amount (the window reset or the budget did not answer)"
		if spent >= 0 {
			drew = fmt.Sprintf("%d", spent)
			if spent > g.cfg.GitHub.UrgentBound {
				drew += ", over"
			}
		}
		return []state.Event{event(g.me, "merge.urgent.spent", "%s drew %s of its own bound %d under the floor", g.key(), drew, g.cfg.GitHub.UrgentBound)}, nil
	})
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
