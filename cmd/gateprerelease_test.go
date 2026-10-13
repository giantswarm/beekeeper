//go:build unix

package cmd

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

// platMgrHR is the platform manager's HelmRelease on gazelle, on the
// stable range platMgrRange.
const (
	platMgrHR    = "flux-giantswarm/giantswarm-platform-manager"
	platMgrRange = ">=0.17.0 <1.0.0"
)

// platMgrOn is platMgrHR Ready on version.
func platMgrOn(version string) merge.HelmRelease {
	return merge.HelmRelease{Key: platMgrHR, Chart: "giantswarm-platform-manager", Version: version, Ready: true, Range: platMgrRange}
}

// A merge in an installation's lane that rolls nothing there frees its lane
// for the next one at once, without the settle wait: a release candidate
// the installation's stable range admits no pre-release of, and a release
// that failed, tagged or not. The merge records why, and a candidate the
// range admits still waits for its roll.
func TestTheNextMergeStartsAtOnceAfterAMergeThatRollsNothing(t *testing.T) {
	const (
		lane    = "portal-tools"
		platMgr = "giantswarm/giantswarm-platform-manager"
		repoMgr = "giantswarm/giantswarm-repo-manager"
	)
	for _, c := range []struct {
		name, doc, rc, merged, held string
	}{
		{"pre-release under a stable range", `{"mergeCommitSha":"abc","release":{"verdict":"available","tag":"v0.69.1-rc.3"}}`, "0",
			platMgr + "#548 exit 0, release v0.69.1-rc.3", ""},
		{"auto-release failed", `{"mergeCommitSha":"abc","release":{"verdict":"ci_failed","tag":""}}`, "6",
			platMgr + "#548 exit 6, release failed, nothing rolls", ""},
		{"tag pipeline failed", `{"mergeCommitSha":"abc","release":{"verdict":"ci_failed","tag":"v0.69.1-rc.3"}}`, "6",
			platMgr + "#548 exit 6, release v0.69.1-rc.3 failed, nothing rolls", ""},
		{"release the range admits", `{"mergeCommitSha":"abc","release":{"verdict":"available","tag":"v0.69.1"}}`, "0",
			platMgr + "#548 exit 0, release v0.69.1", "next in lane portal-tools, waiting for gazelle: " + platMgrHR + " is on 0.69.0, rolling to 0.69.1 (semver " + platMgrRange + ")"},
	} {
		t.Run(c.name, func(t *testing.T) {
			noSystemd(t)
			stubGitHub(t, github.Open, "")
			fakeDevctl(t, `echo '{"repositories":[{"candidate":"v0.33.5-rc.1","state":"dispatched"}]}'`)
			newest := "v0.33.5-rc.1"
			stubCandidate(t, &newest)
			hrs := []merge.HelmRelease{
				platMgrOn("0.69.0"),
				{Key: "flux-giantswarm/giantswarm-repo-manager", Chart: "giantswarm-repo-manager", Version: "0.33.4", Ready: true, Range: platMgrRange},
			}
			read := readHelmReleases
			t.Cleanup(func() { readHelmReleases = read })
			readHelmReleases = func(context.Context, config.Lane) ([]merge.HelmRelease, error) { return hrs, nil }

			now := time.Now()
			a := queueApp(t,
				state.Merge{Repo: platMgr, PR: 548, Lane: lane, By: state.Party{Name: aheadName}, Child: sleeper(t), Phase: state.Running,
					Joined: now.Add(-15 * time.Minute), Started: now.Add(-15 * time.Minute), Roll: []string{platMgrHR}},
				state.Merge{Repo: repoMgr, Lane: lane, By: state.Party{Session: "s1", Name: ownerName}, PID: sleeper(t), Phase: state.Waiting,
					Joined: now.Add(-12 * time.Minute), Seen: now, Candidate: newest})
			a.cfg.Lanes = []config.Lane{{Name: lane, Installation: gazelle, Repositories: []string{platMgr, repoMgr}}}
			a.cfg.Merge.BudgetFresh, a.cfg.Merge.Cap = config.Duration{Duration: time.Hour}, 1000
			a.cfg.Merge.Settle, a.cfg.Merge.SettleTimeout = config.Duration{Duration: 20 * time.Minute}, config.Duration{Duration: 30 * time.Minute}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Budget = &state.Budget{Remaining: 5000, Limit: 5000, Reset: now.Add(time.Hour), At: now}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			st, _ := a.store.Read()
			i := slices.IndexFunc(st.Merges, func(m state.Merge) bool { return m.Repo == repoMgr })
			argv := promoteArgv()
			argv[len(argv)-1] = repoMgr
			next := &gateRun{app: a, ctx: context.Background(), argv: argv, repo: repoMgr,
				lane: a.cfg.LaneOf(repoMgr), me: st.Merges[i].By, pid: os.Getpid(), queued: true, placed: true, from: st.Merges[i].PID}

			endDevctl(t, a, platMgr, 548, c.doc, c.rc)
			why, err := next.step()
			if err != nil || why != c.held {
				t.Fatalf("the next merge: %q, %v, want %q", why, err, c.held)
			}
			if d := lastEventOf(t, a, verbMerged); c.held == "" && !strings.HasPrefix(d, repoMgr+" promote exit 0") {
				t.Errorf("the next merge did not start: %q", d)
			}
			if !slices.ContainsFunc(verbsOf(t, a), func(l string) bool { return strings.HasPrefix(l, verbMerged+" "+c.merged) }) {
				t.Errorf("the merge ahead is not recorded as %q:\n%s", c.merged, strings.Join(verbsOf(t, a), "\n"))
			}
		})
	}
}

// beekeeper lanes names what a settling merge waits for: its release, a
// pre-release, a release unknown, or a release that failed and rolls
// nothing; the watch settles a failed release at once.
func TestLanesSayWhatASettlingMergeWaitsFor(t *testing.T) {
	const (
		lane    = "portal-tools"
		platMgr = "giantswarm/giantswarm-platform-manager"
	)
	stubGitHub(t, github.Open, "")
	hrs := []merge.HelmRelease{platMgrOn("0.69.0")}
	read := readHelmReleases
	t.Cleanup(func() { readHelmReleases = read })
	readHelmReleases = func(context.Context, config.Lane) ([]merge.HelmRelease, error) { return hrs, nil }
	for _, c := range []struct {
		name    string
		release string
		failed  bool
		want    string
	}{
		{"stable release", "v0.69.1", false, "settling " + platMgr + "#548 until v0.69.1 rolls"},
		{"pre-release", "v0.69.1-rc.3", false, "settling " + platMgr + "#548 until pre-release v0.69.1-rc.3 rolls, at once where the range admits no pre-release"},
		{"release unknown", "", false, "settling " + platMgr + "#548, its release unknown, until "},
		{"release failed", "", true, "settled " + platMgr + "#548, release failed: nothing rolls, the next merge starts"},
		{"tagged release failed", "v0.69.1-rc.3", true, "settled " + platMgr + "#548, release v0.69.1-rc.3 failed: nothing rolls, the next merge starts"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := queueApp(t, state.Merge{Repo: platMgr, PR: 548, Lane: lane, By: state.Party{Name: aheadName}, Phase: state.Settling,
				Finished: time.Now(), Release: c.release, ReleaseFailed: c.failed, Roll: []string{platMgrHR}})
			a.cfg.Lanes = []config.Lane{{Name: lane, Installation: gazelle, Repositories: []string{platMgr}}}
			a.cfg.Merge.Settle, a.cfg.Merge.SettleTimeout = config.Duration{Duration: 20 * time.Minute}, config.Duration{Duration: 30 * time.Minute}
			st, _ := a.store.Read()
			out := a.capture(func() { a.printLanes(a.laneViews(st)) })
			if !strings.Contains(out, lane+" ("+gazelle+"): "+c.want) || strings.Contains(out, "unknown release") {
				t.Errorf("lanes:\n%s\nwant %q", out, c.want)
			}
			if !c.failed {
				return
			}
			w := &watcher{app: a, last: map[string]time.Time{}}
			w.settled(context.Background(), a.now)
			if st, _ := a.store.Read(); len(st.Merges) != 0 {
				t.Errorf("the watch keeps a failed release settling: %+v", st.Merges)
			}
			if d := lastEventOf(t, a, verbLaneSettled); d != lane+": "+platMgr+"#548 settled without a roll, "+platMgr+"#548's release failed, nothing rolls" {
				t.Errorf("lane.settled %q", d)
			}
		})
	}
}
