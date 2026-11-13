package cmd

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"syscall"
	"time"

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A lane without an installation has nothing to roll: its merges overlap.
// devctl ends a merge at the merge (--no-release-wait), the merge stays in
// its lane as releasing (state.Merge.Releasing) while its gate confirms the
// release, and the lane's next merge starts meanwhile: it brings its head up
// to the base and waits for its CI beside that release, and merges once the
// release is confirmed or failed. The lane drains one merge per CI run, not
// per CI run and release, and still never merges before the previous
// release's verdict.

// NoneWarranted is a merge's release when its commits warranted none.
const NoneWarranted = "none warranted"

// Seams for the tests.
var (
	// ReleasesPoll is how often a merge that waits for its lane's releases
	// reads the state.
	ReleasesPoll = 5 * time.Second
	// ReleasesWait bounds that wait: past the confirming gate's own release
	// wait (releasePRWait), its settle and a margin, a gate that still
	// confirms is wedged.
	ReleasesWait = func(settle time.Duration) time.Duration { return ReleasePRWait + settle + 5*time.Minute }
)

// OverlapLane says whether the merge's lane lets it start while the lane's
// previous merge confirms its release: a pull request devctl merges, in a
// local lane without an installation, not devctl's own (its release window
// reads the merge's release).
func (g *GateRun) OverlapLane() bool {
	return g.Lane.Installation == "" && g.Pr != 0 && !g.Central && g.RunsDevctl() && !g.ToolMerge()
}

// Overlaps says whether the merge, once merged, confirms its release in its
// gate after devctl ended at the merge, freeing its lane for the next merge
// meanwhile: an overlapLane merge whose release devctl would await, its base
// tagged by Auto-release (no hand cut, no per-plugin tags, no release-PR
// flow, which the gate handles on their own).
func (g *GateRun) Overlaps() bool {
	return g.OverlapLane() && g.handCut() == "" && !g.plugins() && !g.releasePR()
}

// LanePending says what the merge's lane waits for before its next merge
// lands, "" when nothing (merge.Lane.Unsettled without the overlap).
func (g *GateRun) LanePending() string {
	st, err := g.Host.Store().Read()
	if err != nil {
		return fmt.Sprintf("the state does not load (%v)", err)
	}
	return merge.Queue(st, g.Lane.Name).Unsettled(clk.Now(), g.Host.Cfg().Merge.Settle.Duration, false, proc.Alive)
}

// AwaitReleases runs before the merge's devctl in a lane without an
// installation: while a release of the lane is being confirmed it warms the
// pull request up (warmUp, beside the wait), then waits until that release
// is known and the lane settled, so the merge lands after the previous
// release's verdict. The wait is the merge's, its place running without a
// devctl: a stop (SIGINT, a SIGTERM aimed at the run, the gate's context
// ending, beekeeper gate stop) ends it with nothing merged and the place out
// of the lane; a hold that preempts the merge puts it back to waiting
// (preemptedError); a place taken out of the lane refuses; a release still
// unknown past releasesWait refuses rather than merging ahead of its verdict.
// The reviews are read again before devctl starts.
func (g *GateRun) AwaitReleases() error {
	if g.Lane.Installation != "" {
		return nil
	}
	why := g.LanePending()
	if why == "" {
		return nil
	}
	GateLine("lane %s: %s; %s updates its branch and waits for its CI meanwhile, and merges once that release is known",
		g.Lane.Name, why, g.Key())
	ctx, cancel := context.WithCancel(g.Ctx)
	warmed := make(chan struct{})
	go func() {
		defer close(warmed)
		g.WarmUp(ctx)
	}()
	defer func() {
		cancel() // the warm-up ends with the wait: nothing outlives it
		<-warmed
	}()
	wait := ReleasesWait(g.Host.Cfg().Merge.Settle.Duration)
	deadline := clk.Now().Add(wait)
	tick := clk.NewTicker(ReleasesPoll)
	defer tick.Stop()
	said := ""
	for {
		if err := g.stillRunning(); err != nil {
			return err
		}
		if why = g.LanePending(); why == "" {
			break
		}
		if clk.Now().After(deadline) {
			return g.refuseRunning("lane %s still waits for a release's verdict past %s (%s): nothing merged, %s left the lane; run the same command again once beekeeper lanes shows the release known",
				g.Lane.Name, wait, why, g.Key())
		}
		if why != said {
			GateLine("%s waits to merge: %s", g.Key(), why)
			said = why
		}
		select {
		case s := <-g.Sig:
			if err := g.stopWaiting(s); err != nil {
				return err
			}
		case <-g.Ctx.Done():
			return g.stopWaiting(nil)
		case <-tick.C:
		}
	}
	if err := g.stillRunning(); err != nil {
		return err
	}
	GateLine("lane %s: the previous release is known, %s merges", g.Lane.Name, g.Key())
	return g.CheckReviews(g.RefuseRunningWith)
}

// stillRunning says whether the waiting merge may go on: nil while its
// place runs, preemptedError (its place back to waiting) when a hold set
// since lets another pull request through first (merge.Preempts), a
// refusal when its place left the lane.
func (g *GateRun) stillRunning() error {
	st, err := g.Host.Store().Read()
	if err != nil {
		return nil // the next read decides; devctl's start reads it again
	}
	i := g.Mine(st, state.Running)
	if i < 0 {
		return GateRefused("%s was taken out of lane %s while it waited for the lane's release: nothing merged", g.Key(), g.Lane.Name)
	}
	if h, ok := merge.Preempts(st, clk.Now(), st.Merges[i], g.Lane); ok {
		return g.toWaiting(h, "before its devctl started")
	}
	return nil
}

// stopWaiting ends the merge's wait for its lane's release on signal s (nil:
// its context ended), nothing merged, its place out of the lane. A hangup,
// the caller going away, and a SIGTERM from the caller's timeout cut the
// call, not the wait: the merge waits on outside it (nil).
func (g *GateRun) stopWaiting(s os.Signal) error {
	switch {
	case s == syscall.SIGHUP:
		GateLine("hangup: the caller is going away; %s waits on outside it for its lane's release and its outcome reaches its owner", g.Key())
		return nil
	case s == syscall.SIGTERM && TimedCaller():
		GateLine("SIGTERM from the caller's timeout: it cut the connection, not the wait; %s waits on outside the call", g.Key())
		return nil
	}
	code, how := 128+int(syscall.SIGTERM), "its gate's context ended"
	if sig, ok := s.(syscall.Signal); ok {
		code, how = 128+int(sig), sig.String()
	}
	_ = g.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		if i := g.Mine(st, state.Running); i >= 0 {
			st.Merges = slices.Delete(st.Merges, i, i+1)
		}
		return []state.Event{Cmd.Event(g.Me, "merge.stopped", "%s in lane %s: %s while it waited for the lane's release, nothing merged", g.Key(), g.Lane.Name, how)}, nil
	})
	GateLine("%s: %s stopped it while it waited for lane %s's release; nothing merged, it left the lane", g.Key(), how, g.Lane.Name)
	return ExitCode(code)
}

// WarmUp waits for the existing head's CI within the configured timeout.
// It never updates the branch; the merge judges the wait's outcome.
func (g *GateRun) WarmUp(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, g.Host.Cfg().Merge.TimeoutFor(g.Repo))
	defer cancel()
	PrWarmWait(ctx, g.Repo, g.Pr)
}

// ConfirmRelease confirms the release of the merge devctl ended at the
// merge (overlaps), its lane place releasing meanwhile, within
// releasePRWait: an available release or none warranted frees the place;
// a failed release keeps its verdict for settling without a roll, and an
// unconfirmed one settles the lane by the settle rule from
// now. It records the release in out and returns the gate's line and the
// exit devctl pr merge would have ended with.
func (g *GateRun) ConfirmRelease(out *merge.Outcome, rc int) (string, int) {
	GateLine("%s merged; lane %s has no installation to roll, its next merge starts now and merges once this release is known: awaiting it (%s release wait %s --pr %d)",
		g.Key(), g.Lane.Name, merge.Tool, g.Repo, g.Pr)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(g.Ctx), ReleasePRWait)
	defer cancel()
	o, reason, ok := merge.ParseReleaseWait(ReleaseWait(ctx, g.Repo, "--pr", strconv.Itoa(g.Pr)))
	resume := fmt.Sprintf("`%s release wait %s --pr %d` resumes the wait, do not merge again", merge.Tool, g.Repo, g.Pr)
	var line, release string
	switch {
	case !ok:
		out.Unconfirmed, rc = true, DevctlUnconfirmed
		line = fmt.Sprintf("%s merged, and %s release wait ended without its document: its release is unconfirmed, lane %s settles by the settle rule; %s",
			g.Key(), merge.Tool, g.Lane.Name, resume)
	case o.NoRelease:
		out.NoRelease, release = true, NoneWarranted
		line = fmt.Sprintf("%s merged and warranted no release (%s); lane %s is free", g.Key(), reason, g.Lane.Name)
	case o.ReleaseFailed:
		out.Release, out.ReleaseFailed, rc, release = o.Release, true, DevctlReleaseFailed, cmp.Or(o.Release, "release")+" failed"
		line = fmt.Sprintf("%s merged and its release %s failed (%s): nothing rolls, lane %s's next merge lands after this verdict; the fix is the next pull request or a rerun of the failed workflow, do not merge again",
			g.Key(), o.Release, reason, g.Lane.Name)
	case o.Unconfirmed:
		out.Unconfirmed, rc = true, DevctlUnconfirmed
		line = fmt.Sprintf("%s merged, and its release is unconfirmed (%s): lane %s settles by the settle rule; %s", g.Key(), reason, g.Lane.Name, resume)
	default:
		out.Release, release = o.Release, o.Release
		line = fmt.Sprintf("%s merged and released %s, its tag pipeline green; lane %s is free", g.Key(), o.Release, g.Lane.Name)
	}
	now := clk.Now().UTC()
	_ = g.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(st.Merges, func(m state.Merge) bool {
			return m.Repo == g.Repo && m.PR == g.Pr && m.Phase == state.Settling && m.Releasing
		})
		if i < 0 {
			return nil, nil
		}
		deliveryEvents := Cmd.RecordDeliveryRelease(st, g.Repo, g.Pr, *out, st.Merges[i].HandCut, now)
		if out.ReleaseFailed {
			m := &st.Merges[i]
			m.Releasing, m.PID, m.Finished = false, 0, now
			m.Release, m.ReleaseFailed, m.Exit = out.Release, true, rc
			return append(deliveryEvents, []state.Event{Cmd.Event(g.Me, "merge.release", "%s: release %s, lane %s settles without a roll", g.Key(), release, g.Lane.Name)}...), nil
		}
		if out.Unconfirmed {
			st.Merges[i].Releasing, st.Merges[i].Finished = false, now
			return append(deliveryEvents, []state.Event{Cmd.Event(g.Me, "merge.release", "%s: release unconfirmed, lane %s settles by the settle rule", g.Key(), g.Lane.Name)}...), nil
		}
		st.Merges = slices.Delete(st.Merges, i, i+1)
		return append(deliveryEvents, []state.Event{Cmd.Event(g.Me, "merge.release", "%s: release %s, lane %s is free", g.Key(), release, g.Lane.Name)}...), nil
	})
	return line, rc
}
