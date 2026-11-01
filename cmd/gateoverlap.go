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

// noneWarranted is a merge's release when its commits warranted none.
const noneWarranted = "none warranted"

// Seams for the tests.
var (
	// releasesPoll is how often a merge that waits for its lane's releases
	// reads the state.
	releasesPoll = 5 * time.Second
	// releasesWait bounds that wait: past the confirming gate's own release
	// wait (releasePRWait), its settle and a margin, a gate that still
	// confirms is wedged.
	releasesWait = func(settle time.Duration) time.Duration { return releasePRWait + settle + 5*time.Minute }
)

// overlapLane says whether the merge's lane lets it start while the lane's
// previous merge confirms its release: a pull request devctl merges, in a
// local lane without an installation, not devctl's own (its release window
// reads the merge's release).
func (g *gateRun) overlapLane() bool {
	return g.lane.Installation == "" && g.pr != 0 && !g.central && g.runsDevctl() && !g.toolMerge()
}

// overlaps says whether the merge, once merged, confirms its release in its
// gate after devctl ended at the merge, freeing its lane for the next merge
// meanwhile: an overlapLane merge whose release devctl would await, its base
// tagged by Auto-release (no hand cut, no per-plugin tags, no release-PR
// flow, which the gate handles on their own).
func (g *gateRun) overlaps() bool {
	return g.overlapLane() && g.handCut() == "" && !g.plugins() && !g.releasePR()
}

// lanePending says what the merge's lane waits for before its next merge
// lands, "" when nothing (merge.Lane.Unsettled without the overlap).
func (g *gateRun) lanePending() string {
	st, err := g.store.Read()
	if err != nil {
		return fmt.Sprintf("the state does not load (%v)", err)
	}
	return merge.Queue(st, g.lane.Name).Unsettled(time.Now(), g.cfg.Merge.Settle.Duration, false, proc.Alive)
}

// awaitReleases runs before the merge's devctl in a lane without an
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
func (g *gateRun) awaitReleases() error {
	if g.lane.Installation != "" {
		return nil
	}
	why := g.lanePending()
	if why == "" {
		return nil
	}
	gateLine("lane %s: %s; %s updates its branch and waits for its CI meanwhile, and merges once that release is known",
		g.lane.Name, why, g.key())
	ctx, cancel := context.WithCancel(g.ctx)
	warmed := make(chan struct{})
	go func() {
		defer close(warmed)
		g.warmUp(ctx)
	}()
	defer func() {
		cancel() // the warm-up ends with the wait: nothing outlives it
		<-warmed
	}()
	wait := releasesWait(g.cfg.Merge.Settle.Duration)
	deadline := time.Now().Add(wait)
	tick := time.NewTicker(releasesPoll)
	defer tick.Stop()
	said := ""
	for {
		if err := g.stillRunning(); err != nil {
			return err
		}
		if why = g.lanePending(); why == "" {
			break
		}
		if time.Now().After(deadline) {
			return g.refuseRunning("lane %s still waits for a release's verdict past %s (%s): nothing merged, %s left the lane; run the same command again once beekeeper lanes shows the release known",
				g.lane.Name, wait, why, g.key())
		}
		if why != said {
			gateLine("%s waits to merge: %s", g.key(), why)
			said = why
		}
		select {
		case s := <-g.sig:
			if err := g.stopWaiting(s); err != nil {
				return err
			}
		case <-g.ctx.Done():
			return g.stopWaiting(nil)
		case <-tick.C:
		}
	}
	if err := g.stillRunning(); err != nil {
		return err
	}
	gateLine("lane %s: the previous release is known, %s merges", g.lane.Name, g.key())
	return g.checkReviews(g.refuseRunningWith)
}

// stillRunning says whether the waiting merge may go on: nil while its
// place runs, preemptedError (its place back to waiting) when a hold set
// since lets another pull request through first (merge.Preempts), a
// refusal when its place left the lane.
func (g *gateRun) stillRunning() error {
	st, err := g.store.Read()
	if err != nil {
		return nil // the next read decides; devctl's start reads it again
	}
	i := g.mine(st, state.Running)
	if i < 0 {
		return gateRefused("%s was taken out of lane %s while it waited for the lane's release: nothing merged", g.key(), g.lane.Name)
	}
	if h, ok := merge.Preempts(st, time.Now(), st.Merges[i], g.lane); ok {
		return g.toWaiting(h, "before its devctl started")
	}
	return nil
}

// stopWaiting ends the merge's wait for its lane's release on signal s (nil:
// its context ended), nothing merged, its place out of the lane. A hangup,
// the caller going away, and a SIGTERM from the caller's timeout cut the
// call, not the wait: the merge waits on outside it (nil).
func (g *gateRun) stopWaiting(s os.Signal) error {
	switch {
	case s == syscall.SIGHUP:
		gateLine("hangup: the caller is going away; %s waits on outside it for its lane's release and its outcome reaches its owner", g.key())
		return nil
	case s == syscall.SIGTERM && timedCaller():
		gateLine("SIGTERM from the caller's timeout: it cut the connection, not the wait; %s waits on outside the call", g.key())
		return nil
	}
	code, how := 128+int(syscall.SIGTERM), "its gate's context ended"
	if sig, ok := s.(syscall.Signal); ok {
		code, how = 128+int(sig), sig.String()
	}
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		if i := g.mine(st, state.Running); i >= 0 {
			st.Merges = slices.Delete(st.Merges, i, i+1)
		}
		return []state.Event{event(g.me, "merge.stopped", "%s in lane %s: %s while it waited for the lane's release, nothing merged", g.key(), g.lane.Name, how)}, nil
	})
	gateLine("%s: %s stopped it while it waited for lane %s's release; nothing merged, it left the lane", g.key(), how, g.lane.Name)
	return exitCode(code)
}

// warmUp brings the pull request's head up to its base when the merge
// updates its branch (--update-branch; a rebase line by rebase) and waits
// for its CI, within the CI timeout and ctx: devctl's own run then finds the
// head current and judged, and merges at once. Its outcome is devctl's to
// judge, never the warm-up's.
func (g *gateRun) warmUp(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.Merge.CITimeout.Duration)
	defer cancel()
	if merge.UpdatesBranch(g.argv) {
		update := updateBranch
		if merge.Rebase(g.argv) {
			update = updateRebase
		}
		if err := update(ctx, g.repo, g.pr); err != nil {
			gateLine("%s's branch is not updated ahead of its merge (%v): devctl updates it at its turn", g.key(), err)
		}
	}
	prWarmWait(ctx, g.repo, g.pr)
}

// confirmRelease confirms the release of the merge devctl ended at the
// merge (overlaps), its lane place releasing meanwhile, within
// releasePRWait: a known release (available, failed, none warranted) frees
// the place, an unconfirmed one settles the lane by the settle rule from
// now. It records the release in out and returns the gate's line and the
// exit devctl pr merge would have ended with.
func (g *gateRun) confirmRelease(out *merge.Outcome, rc int) (string, int) {
	gateLine("%s merged; lane %s has no installation to roll, its next merge starts now and merges once this release is known: awaiting it (%s release wait %s --pr %d)",
		g.key(), g.lane.Name, merge.Tool, g.repo, g.pr)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(g.ctx), releasePRWait)
	defer cancel()
	o, reason, ok := merge.ParseReleaseWait(releaseWait(ctx, g.repo, "--pr", strconv.Itoa(g.pr)))
	resume := fmt.Sprintf("`%s release wait %s --pr %d` resumes the wait, do not merge again", merge.Tool, g.repo, g.pr)
	var line, release string
	switch {
	case !ok:
		out.Unconfirmed, rc = true, devctlUnconfirmed
		line = fmt.Sprintf("%s merged, and %s release wait ended without its document: its release is unconfirmed, lane %s settles by the settle rule; %s",
			g.key(), merge.Tool, g.lane.Name, resume)
	case o.NoRelease:
		out.NoRelease, release = true, noneWarranted
		line = fmt.Sprintf("%s merged and warranted no release (%s); lane %s is free", g.key(), reason, g.lane.Name)
	case o.ReleaseFailed:
		out.Release, out.ReleaseFailed, rc, release = o.Release, true, devctlReleaseFailed, cmp.Or(o.Release, "release")+" failed"
		line = fmt.Sprintf("%s merged and its release %s failed (%s): nothing rolls, lane %s's next merge lands after this verdict; the fix is the next pull request or a rerun of the failed workflow, do not merge again",
			g.key(), o.Release, reason, g.lane.Name)
	case o.Unconfirmed:
		out.Unconfirmed, rc = true, devctlUnconfirmed
		line = fmt.Sprintf("%s merged, and its release is unconfirmed (%s): lane %s settles by the settle rule; %s", g.key(), reason, g.lane.Name, resume)
	default:
		out.Release, release = o.Release, o.Release
		line = fmt.Sprintf("%s merged and released %s, its tag pipeline green; lane %s is free", g.key(), o.Release, g.lane.Name)
	}
	now := time.Now().UTC()
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(st.Merges, func(m state.Merge) bool {
			return m.Repo == g.repo && m.PR == g.pr && m.Phase == state.Settling && m.Releasing
		})
		if i < 0 {
			return nil, nil
		}
		if out.Unconfirmed {
			st.Merges[i].Releasing, st.Merges[i].Finished = false, now
			return []state.Event{event(g.me, "merge.release", "%s: release unconfirmed, lane %s settles by the settle rule", g.key(), g.lane.Name)}, nil
		}
		st.Merges = slices.Delete(st.Merges, i, i+1)
		return []state.Event{event(g.me, "merge.release", "%s: release %s, lane %s is free", g.key(), release, g.lane.Name)}, nil
	})
	return line, rc
}
