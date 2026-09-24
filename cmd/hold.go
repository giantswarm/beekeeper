package cmd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) holdCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "hold",
		Short: "Stop merges into a repository or a lane, or all GitHub work, until lifted",
		Long: `A hold stops work on a target until it is lifted or its time passes:
"owner/repo" stops merges into that repository (a semantic conflict being
fixed on main), "lane:<name>" (--lane) the merges of one lane (a proving
window, such as a model load on an installation's GPU pool, stops the lane
whose components it exercises and not the others), "merges" every merge,
"github" every GitHub call (the budget is spent). The gate on devctl pr merge
refuses a held merge with the hold's reason. A merge of giantswarm/devctl
opens a tool-release window by itself: a "merges" hold that lets only
giantswarm/devctl through and lifts once the local devctl reports another
version.

Without a subcommand, lists the holds.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return a.holdList() },
	}
	var reason, until, except string
	var lane laneFlag
	set := &cobra.Command{
		Use:   "set <target> | --lane <name>",
		Short: "Hold a target (owner/repo, lane:<name>, merges, github)",
		Args:  lane.args,
		RunE: func(_ *cobra.Command, args []string) error {
			args = lane.target(args)
			if err := a.checkTarget(args[0]); err != nil {
				return err
			}
			if strings.TrimSpace(reason) == "" {
				return &exitError{code: ExitUsage, msg: "--reason is required"}
			}
			u, err := untilTime(a.now, until)
			if err != nil {
				return err
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			h := state.Hold{Target: args[0], Reason: reason, By: me, At: a.now.UTC(), Until: u.UTC(), Except: except}
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Holds = slices.DeleteFunc(st.Holds, func(x state.Hold) bool { return x.Target == h.Target || !x.Active(a.now) })
				st.Holds = append(st.Holds, h)
				return []state.Event{event(me, "hold.set", "%s until %s: %s", h.Target, untilText(a, h), reason)}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "held %s until %s: %s\n", h.Target, untilText(a, h), reason)
			return err
		},
	}
	set.Flags().StringVarP(&reason, "reason", "r", "", "why (required)")
	set.Flags().StringVar(&until, "until", "", "when the hold ends by itself: a time (15:30) or a duration (2h); default: until lifted")
	set.Flags().StringVar(&except, "except", "", "the one owner/repo a merges hold lets through")
	lane.register(set)
	var liftLane, checkLane laneFlag
	lift := &cobra.Command{
		Use:   "lift <target> | --lane <name>",
		Short: "Lift a hold",
		Args:  liftLane.args,
		RunE: func(_ *cobra.Command, args []string) error {
			args = liftLane.target(args)
			me, err := a.caller()
			if err != nil {
				return err
			}
			found := false
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Holds = slices.DeleteFunc(st.Holds, func(x state.Hold) bool {
					if x.Target == args[0] {
						found = true
						return true
					}
					return !x.Active(a.now)
				})
				if !found {
					return nil, nil
				}
				return []state.Event{event(me, "hold.lift", "%s", args[0])}, nil
			})
			if err != nil {
				return err
			}
			if !found {
				_, err = fmt.Fprintf(a.out, "%s was not held\n", args[0])
				return err
			}
			_, err = fmt.Fprintf(a.out, "lifted the hold on %s\n", args[0])
			return err
		},
	}
	check := &cobra.Command{
		Use:   "check <target> | --lane <name>",
		Short: "Exit 0 when the target is not held, 3 when it is",
		Long: `Exit 0 when the target is not held, 3 (with the reason) when it is. A
repository is also held while its lane, "merges" (unless it is the hold's
exception) or "github" is.`,
		Args: checkLane.args,
		RunE: func(_ *cobra.Command, args []string) error {
			args = checkLane.target(args)
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			h, ok := activeHold(st, a, args[0])
			if strings.Contains(args[0], "/") {
				h, ok = merge.Blocking(st, a.now, args[0], a.cfg.LaneOf(args[0]).Name)
			}
			if ok {
				return refused("%s is held by %q until %s: %s", h.Target, h.By.Name, untilText(a, h), h.Reason)
			}
			return nil
		},
	}
	liftLane.register(lift)
	checkLane.register(check)
	list := listCmd("List the holds", a.holdList)
	c.AddCommand(set, lift, check, list)
	return c
}

// laneFlag is --lane <name>, which stands for the target lane:<name>.
type laneFlag struct{ name string }

func (l *laneFlag) register(c *cobra.Command) {
	c.Flags().StringVar(&l.name, "lane", "", "the lane to hold (the target lane:<name>)")
}

func (l *laneFlag) args(_ *cobra.Command, args []string) error {
	if l.name != "" {
		return cobra.NoArgs(nil, args)
	}
	return cobra.ExactArgs(1)(nil, args)
}

func (l *laneFlag) target(args []string) []string {
	if l.name != "" {
		return []string{merge.LanePrefix + l.name}
	}
	return args
}

// checkTarget refuses a hold on a lane the configuration does not name.
func (a *app) checkTarget(target string) error {
	if name, ok := strings.CutPrefix(target, merge.LanePrefix); ok {
		if _, known := a.cfg.LaneNamed(name); !known {
			return usageErr("no lane %q in the configuration (beekeeper lanes lists them)", name)
		}
	}
	return nil
}

// activeHold returns the hold that applies to target: its own, or the
// "github" hold for any target.
func activeHold(st *state.State, a *app, target string) (state.Hold, bool) {
	for _, t := range []string{target, "github"} {
		for _, h := range st.Holds {
			if h.Target == t && h.Active(a.now) {
				return h, true
			}
		}
	}
	return state.Hold{}, false
}

func untilText(a *app, h state.Hold) string {
	if h.Until.IsZero() {
		return "lifted"
	}
	return clock(a.now, h.Until)
}

func (a *app) activeHolds(st *state.State) []state.Hold {
	var out []state.Hold
	for _, h := range st.Holds {
		if h.Active(a.now) {
			out = append(out, h)
		}
	}
	return out
}

func (a *app) holdList() error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	holds := a.activeHolds(st)
	if a.json {
		return a.printJSON(holds)
	}
	a.printHolds(holds)
	return nil
}

func (a *app) printHolds(holds []state.Hold) {
	if len(holds) == 0 {
		_, _ = fmt.Fprintln(a.out, "nothing is held")
		return
	}
	w := a.table()
	_, _ = fmt.Fprintln(w, "TARGET\tUNTIL\tBY\tSINCE\tREASON")
	for _, h := range holds {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", h.Target, untilText(a, h), truncate(h.By.Name, 30), clock(a.now, h.At), truncate(h.Reason, 60))
	}
	_ = w.Flush()
}
