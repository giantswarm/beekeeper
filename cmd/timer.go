package cmd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) timerCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "timer",
		Short: "Times to look at something: check the rollout after 22:55",
		Long: `Timers are the points in time a supervisor would otherwise keep in its
transcript ("check the rollout after 22:55"). beekeeper watch prints one line
when a timer is due; it stays in every hand-over until marked done.

Without a subcommand, lists the open timers.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return a.timerList() },
	}
	add := &cobra.Command{
		Use:   "add <time> <what>",
		Short: "Add a timer: a time (22:55) or a duration (45m), and what to look at",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			due, err := untilTime(a.now, args[0])
			if err != nil {
				return err
			}
			if due.IsZero() {
				return usageErr("a timer needs a time")
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			t := state.Timer{Due: due.UTC(), What: strings.Join(args[1:], " "), By: me, At: a.now.UTC()}
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.NextTimer++
				t.ID = st.NextTimer
				st.Timers = append(st.Timers, t)
				return []state.Event{event(me, "timer.add", "#%d at %s: %s", t.ID, clock(a.now, t.Due), t.What)}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "timer #%d at %s\n", t.ID, clock(a.now, t.Due))
			return err
		},
	}
	done := &cobra.Command{
		Use:   "done <id>...",
		Short: "Mark timers done",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ids, err := parseIDs(args, "timer")
			if err != nil {
				return err
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			var evs []state.Event
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Timers = slices.DeleteFunc(st.Timers, func(t state.Timer) bool {
					if slices.Contains(ids, t.ID) {
						evs = append(evs, event(me, "timer.done", "#%d %s", t.ID, t.What))
						return true
					}
					return false
				})
				return evs, nil
			})
			if err != nil {
				return err
			}
			if len(evs) < len(ids) {
				return refused("%d of the %d timers were open", len(evs), len(ids))
			}
			return nil
		},
	}
	c.AddCommand(add, done, listCmd("List the open timers", a.timerList))
	return c
}

func (a *app) timerList() error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(st.Timers)
	}
	a.printTimers(st.Timers)
	return nil
}

func (a *app) printTimers(timers []state.Timer) {
	if len(timers) == 0 {
		_, _ = fmt.Fprintln(a.out, "no open timers")
		return
	}
	for _, t := range timers {
		when := "at " + clock(a.now, t.Due)
		if !t.Fired.IsZero() {
			when += " (due)"
		}
		_, _ = fmt.Fprintf(a.out, "#%d [%s, set by %s] %s\n", t.ID, when, t.By.Name, t.What)
	}
}
