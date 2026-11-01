package cmd

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
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
// pull request up (warmUp), then waits until that release is known and the
// lane settled, so the merge lands after the previous release's verdict.
func (g *gateRun) awaitReleases() {
	if g.lane.Installation != "" {
		return
	}
	why := g.lanePending()
	if why == "" {
		return
	}
	gateLine("lane %s: %s; %s updates its branch and waits for its CI meanwhile, and merges once that release is known",
		g.lane.Name, why, g.key())
	g.warmUp()
	deadline := time.Now().Add(releasesWait(g.cfg.Merge.Settle.Duration))
	said := ""
	for why = g.lanePending(); why != ""; why = g.lanePending() {
		if time.Now().After(deadline) {
			gateLine("lane %s still waits past %s (%s): %s merges now, the confirming gate is wedged", g.lane.Name,
				releasesWait(g.cfg.Merge.Settle.Duration), why, g.key())
			return
		}
		if why != said {
			gateLine("%s waits to merge: %s", g.key(), why)
			said = why
		}
		time.Sleep(releasesPoll)
	}
	gateLine("lane %s: the previous release is known, %s merges", g.lane.Name, g.key())
}

// warmUp brings the pull request's head up to its base when the merge
// updates its branch (--update-branch; a rebase line by rebase) and waits
// for its CI: devctl's own run then finds the head current and judged, and
// merges at once. Its outcome is devctl's to judge, never the warm-up's.
func (g *gateRun) warmUp() {
	ctx, cancel := context.WithTimeout(g.ctx, g.cfg.Merge.CITimeout.Duration)
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
