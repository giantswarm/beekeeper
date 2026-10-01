package cmd

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) timerCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "timer",
		Short: "Times to look at something: check the rollout after 22:55",
		Long: `Timers are the points in time a supervisor would otherwise keep in its
transcript ("check the rollout after 22:55"). beekeeper watch prints one line
when a timer is due; it stays in every hand-over until marked done. A timer
can wait on a condition and wake an agent or run a command itself (timer
add --help); such a timer closes when it fires.

Without a subcommand, lists the open timers.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return a.timerList() },
	}
	var spec timerSpec
	add := &cobra.Command{
		Use:   "add <time> <what>",
		Short: "Add a timer: a time (22:55) or a duration (45m), and what to look at",
		Long: `Adds a timer at a time (22:55) or after a duration (45m).

A plain timer is one watch line when its time comes and stays open until
timer done. With --when or --probe it waits on a condition from its time:
the watch checks it every --every (5m) and fires the timer once it holds,
within one tick. --until ends the wait: past it the timer fires as timed
out, or with --expire closes unfired with one line. --wake names the agent
the firing wakes with the timer's text (as agents wake does); --run runs a
shell command instead, its output in timers/<id>.log of the state
directory and its exit code logged (timer.ran). Without either the watch's
line says it. A timer with a condition, --wake or --run closes when it
fires.

The conditions of --when, each one read of its reference per check, shared
by every timer on it; a GitHub one waits while the budget is under the
floor:
  pr-merged owner/repo#n
  issue-closed owner/repo#n
  helmrelease-ready context/namespace/name
  controlplane-ready context/namespace/name   (every replica ready and updated)
--probe takes any shell command that exits 0 once the condition holds.`,
		Example: `  beekeeper timer add 5m "PR 243 merged: rebase and merge yours" --when "pr-merged giantswarm/beekeeper#243" --wake "BK 229" --until 2h
  beekeeper timer add 23:00 "swap flat again" --probe "test $(awk '/SwapFree/{print $2}' /proc/meminfo) -gt 8000000" --until 06:00 --expire
  beekeeper timer add 11:01 "budget reset" --run "beekeeper agents wake 'BK 228' 'the budget is back'"`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			due, err := untilTime(a.now, args[0])
			if err != nil {
				return err
			}
			if due.IsZero() {
				return usageErr("a timer needs a time")
			}
			t, err := a.timerFrom(spec, due)
			if err != nil {
				return err
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			t.What, t.By, t.At = strings.Join(args[1:], " "), me, a.now.UTC()
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.NextTimer++
				t.ID = st.NextTimer
				st.Timers = append(st.Timers, t)
				return []state.Event{event(me, "timer.add", "#%d %s: %s", t.ID, timerWhen(a.now, t), t.What)}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "timer #%d %s\n", t.ID, timerWhen(a.now, t))
			return err
		},
	}
	f := add.Flags()
	f.StringVar(&spec.when, "when", "", "a condition to wait on from the time: pr-merged, issue-closed, helmrelease-ready, controlplane-ready and its reference")
	f.StringVar(&spec.probe, "probe", "", "a shell command to wait on from the time: it exits 0 once the condition holds")
	f.DurationVar(&spec.every, "every", 0, "how often the condition is checked (default 5m)")
	f.StringVar(&spec.until, "until", "", "the end of the wait, a time or a duration from now: the timer fires as timed out")
	f.BoolVar(&spec.expire, "expire", false, "at --until close the timer unfired instead")
	f.StringVar(&spec.wake, "wake", "", "the registered agent the firing wakes with the timer's text")
	f.StringVar(&spec.run, "run", "", "a shell command the firing runs, its exit code logged")
	add.MarkFlagsMutuallyExclusive("when", "probe")
	add.MarkFlagsMutuallyExclusive("wake", "run")
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
		when := timerWhen(a.now, t)
		if !t.Fired.IsZero() {
			when += " (due)"
		}
		_, _ = fmt.Fprintf(a.out, "#%d [%s, set by %s] %s\n", t.ID, when, t.By.Name, t.What)
	}
}

// timerSpec is what timer add was given beyond the time and the text.
type timerSpec struct {
	when, probe, until, wake, run string
	every                         time.Duration
	expire                        bool
}

// timerFrom checks spec and returns the timer due at due it describes.
func (a *app) timerFrom(spec timerSpec, due time.Time) (state.Timer, error) {
	t := state.Timer{Due: due.UTC(), When: strings.TrimSpace(spec.when), Probe: strings.TrimSpace(spec.probe), Every: spec.every, Expire: spec.expire, Run: strings.TrimSpace(spec.run)}
	if t.When != "" {
		if _, _, err := conditionProbe(t.When); err != nil {
			return t, err
		}
	}
	until, err := untilTime(a.now, spec.until)
	if err != nil {
		return t, err
	}
	t.Until = until.UTC()
	switch {
	case !t.Conditional() && (spec.every != 0 || spec.until != "" || spec.expire):
		return t, usageErr("--every, --until and --expire need a condition: --when or --probe")
	case spec.every < 0:
		return t, usageErr("--every %s: a check needs a positive interval", spec.every)
	case spec.expire && spec.until == "":
		return t, usageErr("--expire needs --until")
	case !t.Until.IsZero() && !t.Until.After(t.Due):
		return t, usageErr("--until %s is not after the timer's time %s", clock(a.now, t.Until), clock(a.now, t.Due))
	}
	if q := strings.TrimSpace(spec.wake); q != "" {
		st, err := a.store.Read()
		if err != nil {
			return t, err
		}
		i, err := findAgent(st, q)
		if err != nil {
			return t, err
		}
		t.Wake = st.Agents[i].Name
	}
	return t, nil
}

// timerWhen says when a timer fires and what it does then.
func timerWhen(now time.Time, t state.Timer) string {
	s := "at " + clock(now, t.Due)
	if t.Conditional() {
		s = "from " + clock(now, t.Due) + " when " + timerCond(t)
		if t.Every != 0 {
			s += ", every " + t.Every.String()
		}
		if !t.Until.IsZero() {
			s += ", until " + clock(now, t.Until)
			if t.Expire {
				s += " (then expires)"
			}
		}
	}
	switch {
	case t.Wake != "":
		s += fmt.Sprintf(", wakes %q", t.Wake)
	case t.Run != "":
		s += ", runs `" + truncate(t.Run, 60) + "`"
	}
	return s
}
