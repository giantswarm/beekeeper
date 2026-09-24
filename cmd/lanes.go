package cmd

import (
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

// laneView is one lane's queue with its installation and hold.
type laneView struct {
	merge.Lane
	Installation string      `json:"installation,omitempty"`
	Hold         *state.Hold `json:"hold,omitempty"`
}

func (a *app) lanesCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "lanes",
		Short: "The merge lanes: the running merge, the settling one and who is next",
		Long: `A lane is a set of repositories whose merges roll the same components of an
installation (lanes in the configuration; a repository in no lane is a lane
of its own). The gate the PreToolUse hook puts in front of devctl pr merge
runs one merge per lane at a time, in the order the merges joined, and the
next once the installation's HelmReleases of the lane's charts are Ready and
the previous merge's release has rolled.

Without a subcommand, lists every configured lane and every other lane with
a merge in it: running, settling, and the waiting merges in turn order.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			views := a.laneViews(st)
			if a.json {
				return a.printJSON(views)
			}
			a.printLanes(views)
			return nil
		},
	}
	c.AddCommand(&cobra.Command{
		Use:   "clear <lane>",
		Short: "Free a lane whose settling merge will not roll",
		Long: `clear drops the lane's settling merge, after its installation was checked
by hand: a release that is not going to roll (a HelmRelease pinned since,
a failed upgrade rolled back) otherwise refuses the lane's next merge once
merge.settleTimeout has passed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			var cleared []string
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool {
					if m.Lane == args[0] && m.Phase == state.Settling {
						cleared = append(cleared, m.Key())
						return true
					}
					return false
				})
				if len(cleared) == 0 {
					return nil, nil
				}
				return []state.Event{event(me, "lane.clear", "%s: %v", args[0], cleared)}, nil
			})
			if err != nil {
				return err
			}
			if len(cleared) == 0 {
				_, err = fmt.Fprintf(a.out, "nothing settles in lane %s\n", args[0])
				return err
			}
			_, err = fmt.Fprintf(a.out, "cleared lane %s: %v no longer settles\n", args[0], cleared)
			return err
		},
	})
	return c
}

// laneViews are the configured lanes, then every other lane with a merge.
func (a *app) laneViews(st *state.State) []laneView {
	var out []laneView
	seen := map[string]bool{}
	add := func(name, installation string) {
		if seen[name] {
			return
		}
		seen[name] = true
		v := laneView{Lane: merge.Queue(st, name), Installation: installation}
		for _, h := range st.Holds {
			if h.Target == merge.LanePrefix+name && h.Active(a.now) {
				v.Hold = &h
			}
		}
		out = append(out, v)
	}
	for _, l := range a.cfg.Lanes {
		add(l.Name, l.Installation)
	}
	for _, m := range st.Merges {
		add(m.Lane, a.cfg.LaneOf(m.Repo).Installation)
	}
	return out
}

func (a *app) printLanes(views []laneView) {
	if len(views) == 0 {
		_, _ = fmt.Fprintln(a.out, "no lanes are configured and nothing merges")
		return
	}
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
	for _, v := range views {
		where := ""
		if v.Installation != "" {
			where = " (" + v.Installation + ")"
		}
		status := "free"
		switch {
		case v.Running != nil:
			status = fmt.Sprintf("running %s by %q since %s", v.Running.Key(), v.Running.By.Name, clock(a.now, v.Running.Started))
		case v.Settling != nil:
			release := v.Settling.Release
			if release == "" {
				release = "an unknown release"
			}
			status = fmt.Sprintf("settling %s until %s rolls", v.Settling.Key(), release)
		}
		p("%s%s: %s", v.Name, where, status)
		if v.Hold != nil {
			p("  held by %q until %s: %s", v.Hold.By.Name, untilText(a, *v.Hold), v.Hold.Reason)
		}
		for i, m := range v.Waiting {
			label := "      "
			if i == 0 {
				label = "  next"
			}
			p("%s %d. %s by %q, waiting since %s", label, i+1, m.Key(), m.By.Name, clock(a.now, m.Joined))
		}
	}
}
