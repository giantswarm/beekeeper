package cmd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) holdCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "hold",
		Short: "Stop merges into a repository, or all GitHub work, until lifted",
		Long: `A hold stops work on a target until it is lifted or its time passes:
"owner/repo" stops merges into that repository (a proving window on an
installation that follows it by semver, a semantic conflict being fixed on
main), "github" stops every GitHub call (the budget is spent). Sessions check
before they act: ` + "`beekeeper hold check <target> && devctl pr merge …`" + `.

Without a subcommand, lists the holds.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return a.holdList() },
	}
	var reason, until string
	set := &cobra.Command{
		Use:   "set <target>",
		Short: "Hold a target (owner/repo, github)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
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
			h := state.Hold{Target: args[0], Reason: reason, By: me, At: a.now.UTC(), Until: u.UTC()}
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
	lift := &cobra.Command{
		Use:   "lift <target>",
		Short: "Lift a hold",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
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
		Use:   "check <target>",
		Short: "Exit 0 when the target is not held, 3 when it is",
		Long: `Exit 0 when the target is not held, 3 (with the reason) when it is. A
repository is also held while "github" is.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			if h, ok := activeHold(st, a, args[0]); ok {
				return refused("%s is held by %q until %s: %s", h.Target, h.By.Name, untilText(a, h), h.Reason)
			}
			return nil
		},
	}
	list := listCmd("List the holds", a.holdList)
	c.AddCommand(set, lift, check, list)
	return c
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
