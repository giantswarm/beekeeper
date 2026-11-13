package cmd

import (
	"fmt"
	"slices"
	"time"

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/state"
)

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
	bound := g.a.cfg.GitHub.UrgentBound
	var refusal string
	err = g.a.store.Update(func(st *state.State) ([]state.Event, error) {
		pruneUrgent(st, g.a.now, g.a.cfg.Merge.SeedTTL.Duration)
		i := slices.IndexFunc(st.Urgent, func(u state.Urgent) bool { return u.Key() == g.key() })
		if i < 0 {
			return nil, nil
		}
		u := &st.Urgent[i]
		if refusal = urgentTaken(g.a, st, g.key()); refusal != "" {
			st.Urgent = slices.Delete(st.Urgent, i, i+1)
			return []state.Event{event(g.me, "merge.urgent.refused", "%s: %s; it waits for the reset", g.key(), refusal)}, nil
		}
		if b.Remaining < bound {
			why = fmt.Sprintf("%s is urgent (asked by %q), and the GitHub budget %d is under its own bound %d until the reset at %s",
				g.key(), u.By.Name, b.Remaining, bound, clock(g.a.now, b.Reset))
			return nil, nil
		}
		run = true
		u.Window, u.Remaining = b.Reset.UTC(), b.Remaining
		return []state.Event{event(g.me, "merge.urgent", "%s runs under the floor (budget %d, floor %d, its own bound %d) as the urgent merge of the window resetting at %s, asked by %q: %s",
			g.key(), b.Remaining, g.a.cfg.GitHub.Floor, bound, clock(g.a.now, b.Reset), u.By.Name, u.Reason)}, nil
	})
	if refusal != "" {
		gateLine("urgent refused, %s: %s waits for the reset like any other", refusal, g.key())
	}
	if run {
		gateLine("urgent: %s runs under the budget floor (%d of the floor %d, its own bound %d), once in this reset window", g.key(), b.Remaining, g.a.cfg.GitHub.Floor, bound)
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
		g.a.now = clk.Now()
		if after, err := g.gateBudget(); err == nil && after.Reset.Equal(before.Reset) {
			spent = before.Remaining - after.Remaining
		}
	}
	_ = g.a.store.Update(func(st *state.State) ([]state.Event, error) {
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
			if spent > g.a.cfg.GitHub.UrgentBound {
				drew += ", over"
			}
		}
		return []state.Event{event(g.me, "merge.urgent.spent", "%s drew %s of its own bound %d under the floor", g.key(), drew, g.a.cfg.GitHub.UrgentBound)}, nil
	})
}
