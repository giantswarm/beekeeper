package gate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/state"
)

// centralEvery is how often a merge waiting in a central lane asks the
// central instance for its turn; the answer is kept in between.
const centralEvery = 15 * time.Second

// CentralTurn queues the merge in its central lane, or keeps its place
// there, and returns what it waits for there: "" on its turn. A hold the
// central instance names refuses the merge, as a local hold does; an
// unreachable central instance refuses it with ExitCentral.
func (g *GateRun) CentralTurn() (string, error) {
	every := centralEvery
	if ttl := g.Host.Cfg().Merge.QueueTTL.Duration; ttl > 0 {
		every = min(every, ttl/2)
	}
	if g.centralWhy != "" && time.Since(g.CentralAsked) < every {
		return g.centralWhy, nil
	}
	var t LaneTurn
	_, err := g.Host.CallCentralRaw(g.Ctx, g.Me, "lane_queue", g.prArgs(), &t)
	g.CentralAsked, g.centralWhy = time.Now(), ""
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
func (g *GateRun) centralRefusal(err error) error {
	var r *central.Refused
	var u *central.Unreachable
	switch {
	case err == nil:
		return nil
	case errors.As(err, &r):
		return g.Refuse("%s", r.Message)
	case errors.As(err, &u):
		return g.RefuseWith(ExitCentral, "%s; the merge is not queued: run the same command again once it answers", u.Error())
	}
	return g.Refuse("lane %s: %v", g.Lane.Name, err)
}

// CentralStart makes the merge, running here, its central lane's running
// one. When another merge of the lane started there first, the merge waits
// again here (unstart) and it returns what it waits for.
func (g *GateRun) CentralStart() (string, error) {
	_, err := g.Host.CallCentralRaw(g.Ctx, g.Me, "lane_settle", g.prArgs(), nil)
	if err == nil {
		return "", nil
	}
	g.unstart()
	var r *central.Refused
	if errors.As(err, &r) {
		g.centralWhy, g.CentralAsked = r.Message, time.Now()
		return r.Message, nil
	}
	return "", g.centralRefusal(err)
}

// CentralRecord tells the central lane how the merge's run ended: merged,
// the lane settles until the machine's watch saw its release roll;
// otherwise it leaves the lane. A failure is said, never fatal: devctl ran.
func (g *GateRun) CentralRecord(settles bool, why string) {
	args, tool := g.prArgs(), "lane_leave"
	if settles {
		args["merged"], tool = true, "lane_settle"
	} else {
		args[paramReason] = why
	}
	if _, err := g.Host.CallCentralRaw(context.WithoutCancel(g.Ctx), g.Me, tool, args, nil); err != nil {
		GateLine("central lane %s was not told of %s's outcome (%v): its place there stays until `beekeeper lanes leave %s`",
			g.Lane.Name, g.Key(), err, g.Key())
	}
}

// CentralLeave takes the merge out of its central lane once it joined.
func (g *GateRun) CentralLeave(why string) {
	if !g.joined {
		return
	}
	args := g.prArgs()
	args[paramReason] = why
	_, _ = g.Host.CallCentralRaw(context.WithoutCancel(g.Ctx), g.Me, "lane_leave", args, nil)
	g.joined = false
}

func (g *GateRun) prArgs() map[string]any {
	return map[string]any{paramRepo: g.Repo, "pr": g.Pr}
}
