package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/state"
)

// centralEvery is how often a merge waiting in a central lane asks the
// central instance for its turn; the answer is kept in between.
const centralEvery = 15 * time.Second

// centralTurn queues the merge in its central lane, or keeps its place
// there, and returns what it waits for there: "" on its turn. A hold the
// central instance names refuses the merge, as a local hold does; an
// unreachable central instance refuses it with ExitCentral.
func (g *gateRun) centralTurn() (string, error) {
	if g.centralWhy != "" && time.Since(g.centralAsked) < centralEvery {
		return g.centralWhy, nil
	}
	var t laneTurn
	_, err := g.callCentralRaw(g.ctx, g.me, "lane_queue", g.prArgs(), &t)
	g.centralAsked, g.centralWhy = time.Now(), ""
	if err := g.centralRefusal(err); err != nil {
		return "", err
	}
	g.joined = true
	switch {
	case t.Turn:
		return "", nil
	case t.Phase != state.Waiting:
		g.centralWhy = fmt.Sprintf("%s is %s in central lane %s", t.Merge, t.Phase, t.Lane)
	case len(t.Ahead) == 0:
		g.centralWhy = fmt.Sprintf("next in central lane %s once its running or settling merge frees it", t.Lane)
	default:
		g.centralWhy = fmt.Sprintf("position %d in central lane %s behind %s", t.Position, t.Lane, strings.Join(t.Ahead, ", "))
	}
	return g.centralWhy, nil
}

// centralRefusal is the gate's refusal of a central call's failure, nil
// when it succeeded.
func (g *gateRun) centralRefusal(err error) error {
	var r *central.Refused
	var u *central.Unreachable
	switch {
	case err == nil:
		return nil
	case errors.As(err, &r):
		return g.refuse("%s", r.Message)
	case errors.As(err, &u):
		return g.refuseWith(ExitCentral, "%s; the merge is not queued: run the same command again once it answers", u.Error())
	}
	return g.refuse("lane %s: %v", g.lane.Name, err)
}

// centralStart makes the merge, running here, its central lane's running
// one. When another merge of the lane started there first, the merge waits
// again here (unstart) and it returns what it waits for.
func (g *gateRun) centralStart() (string, error) {
	_, err := g.callCentralRaw(g.ctx, g.me, "lane_settle", g.prArgs(), nil)
	if err == nil {
		return "", nil
	}
	g.unstart()
	var r *central.Refused
	if errors.As(err, &r) {
		g.centralWhy, g.centralAsked = r.Message, time.Now()
		return r.Message, nil
	}
	return "", g.centralRefusal(err)
}

// centralRecord tells the central lane how the merge's run ended: merged,
// the lane settles until the machine's watch saw its release roll;
// otherwise it leaves the lane. A failure is said, never fatal: devctl ran.
func (g *gateRun) centralRecord(settles bool, why string) {
	args, tool := g.prArgs(), "lane_leave"
	if settles {
		args["merged"], tool = true, "lane_settle"
	} else {
		args[paramReason] = why
	}
	if _, err := g.callCentralRaw(context.WithoutCancel(g.ctx), g.me, tool, args, nil); err != nil {
		gateLine("central lane %s was not told of %s's outcome (%v): its place there stays until `beekeeper lanes leave %s`",
			g.lane.Name, g.key(), err, g.key())
	}
}

// centralLeave takes the merge out of its central lane once it joined.
func (g *gateRun) centralLeave(why string) {
	if !g.joined {
		return
	}
	args := g.prArgs()
	args[paramReason] = why
	_, _ = g.callCentralRaw(context.WithoutCancel(g.ctx), g.me, "lane_leave", args, nil)
	g.joined = false
}

func (g *gateRun) prArgs() map[string]any {
	return map[string]any{paramRepo: g.repo, "pr": g.pr}
}

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
