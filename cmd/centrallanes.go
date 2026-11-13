package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/state"
)

// leaveCentral takes merges that settled on this machine out of their
// central lanes, as their parties.
func (a *app) leaveCentral(ctx context.Context, merges []state.Merge, why string) {
	for _, m := range merges {
		l, ok := a.cfg.LaneNamed(m.Lane)
		if !ok || !a.cfg.CentralLane(l) || m.PR == 0 {
			continue
		}
		_, _ = a.callCentralRaw(ctx, state.Party{Name: m.By.Name, Host: a.cfg.Identity.Host}, "lane_leave",
			map[string]any{paramRepo: m.Repo, "pr": m.PR, paramReason: why}, nil)
	}
}

// centralLanesCmd lists the central instance's lanes.
func (a *app) centralLanesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "central",
		Short: "The central instance's lanes: their running and settling merges and queues",
		Long: `The lanes of the installations the central instance holds (central in the
configuration; a lane whose installation is central). The gate queues a merge
of such a lane there as well as here, and runs it only when it is the next
in both: two people's merges into one lane roll one after the other.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if !a.cfg.Central.Enabled() {
				return usageErr("no central instance is configured (central.context)")
			}
			me, _ := a.caller()
			var v struct {
				Lanes []centralLane `json:"lanes"`
			}
			text, err := a.callCentral(context.Background(), me, lanesName, map[string]any{}, &v)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(v.Lanes)
			}
			_, err = fmt.Fprintln(a.out, strings.TrimSpace(text))
			return err
		},
	}
}

// laneLeaveCmd takes a merge out of its central lane.
func (a *app) laneLeaveCmd() *cobra.Command {
	var reason string
	c := &cobra.Command{
		Use:   "leave <owner/repo#n>",
		Short: "Take a merge out of its central lane (its person's, or the team's supervisor role's)",
		Long: `leave takes a merge out of its central lane: one whose gate could not tell
the central instance how it ended, or whose release was checked by hand.
Only the merge's person, or the supervisor role of their team, may.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			repo, pr, ok := splitPR(args[0])
			if !ok || pr == 0 {
				return usageErr("%s: owner/repo#n", args[0])
			}
			if !a.cfg.CentralLane(a.cfg.LaneOf(repo)) {
				return usageErr("%s's lane is not central: lanes drop takes a merge out of a local lane", repo)
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			text, err := a.callCentral(context.Background(), me, "lane_leave", map[string]any{paramRepo: repo, "pr": pr, paramReason: reason}, nil)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, strings.TrimSpace(text))
			return err
		},
	}
	c.Flags().StringVar(&reason, "reason", "left by hand", "why it leaves")
	return c
}
