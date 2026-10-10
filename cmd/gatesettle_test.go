//go:build unix

package cmd

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

// endDevctl writes the files a merge's devctl leaves when it ends: its
// document, when it printed one, and its exit code.
func endDevctl(t *testing.T, a *app, repo string, pr int, doc, rc string) {
	t.Helper()
	base := mergeBase(a.store.Dir(), repo, pr)
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	if doc != "" {
		if err := os.WriteFile(base+".json", []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(base+".rc", []byte(rc+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// verbsOf are the logged verbs and details, in order.
func verbsOf(t *testing.T, a *app) []string {
	t.Helper()
	evs, err := a.store.Events(0, func(state.Event) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, e.Verb+" "+e.Detail)
	}
	return out
}

// indexOf is the index of the first line with prefix, -1 when none.
func indexOf(lines []string, prefix string) int {
	return slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, prefix) })
}

// Two promotions queued behind a running merge in an installation's lane:
// the first waits while the merge runs, its gate gone past the tool limit
// and its devctl on, and starts only once the merge merged, its release
// candidate cut and the lane settled (a candidate the installation does
// not follow settles it without a roll); the second waits behind the first.
func TestAPromotionWaitsForTheRunningMergeAheadToSettle(t *testing.T) {
	noSystemd(t)
	stubGitHub(t, github.Open, "")
	const (
		lane      = "portal-tools"
		backstage = "giantswarm/backstage"
		repoMgr   = "giantswarm/giantswarm-repo-manager"
		platMgr   = "giantswarm/giantswarm-platform-manager"
		candidate = "v0.33.5-rc.1"
	)
	fakeDevctl(t, `echo '{"repositories":[{"candidate":"`+candidate+`","state":"dispatched"}]}'`)
	newest := candidate
	stubCandidate(t, &newest)
	hrs := []merge.HelmRelease{
		{Key: "flux-giantswarm/backstage", Chart: "backstage", Version: "2.95.0", Ready: true, Range: ">=2.1.0 <3.0.0"},
		{Key: "flux-giantswarm/giantswarm-repo-manager", Chart: "giantswarm-repo-manager", Version: "0.33.4", Ready: true, Range: ">=0.17.0 <1.0.0"},
		{Key: "flux-giantswarm/giantswarm-platform-manager", Chart: "giantswarm-platform-manager", Version: "0.70.0", Ready: true, Range: ">=0.17.0 <1.0.0"},
	}
	read := readHelmReleases
	t.Cleanup(func() { readHelmReleases = read })
	readHelmReleases = func(context.Context, config.Lane) ([]merge.HelmRelease, error) { return hrs, nil }

	now := time.Now()
	me := state.Party{Session: "s1", Name: ownerName}
	a := queueApp(t,
		// The merge's gate left at the tool limit; its devctl runs on.
		state.Merge{Repo: backstage, PR: 2885, Lane: lane, By: state.Party{Name: aheadName}, Child: sleeper(t), Phase: state.Running,
			Joined: now.Add(-15 * time.Minute), Started: now.Add(-15 * time.Minute), Roll: []string{"flux-giantswarm/backstage"}},
		state.Merge{Repo: repoMgr, Lane: lane, By: me, PID: sleeper(t), Phase: state.Waiting,
			Joined: now.Add(-12 * time.Minute), Seen: now, Candidate: candidate},
		state.Merge{Repo: platMgr, Lane: lane, By: state.Party{Name: "second"}, PID: sleeper(t), Phase: state.Waiting,
			Joined: now.Add(-10 * time.Minute), Seen: now, Candidate: "v0.70.1-rc.1"})
	a.cfg.Lanes = []config.Lane{{Name: lane, Installation: gazelle, Repositories: []string{backstage, repoMgr, platMgr}}}
	a.cfg.Merge.BudgetFresh, a.cfg.Merge.Cap = config.Duration{Duration: time.Hour}, 1000
	a.cfg.Merge.Settle, a.cfg.Merge.SettleTimeout = config.Duration{Duration: 5 * time.Minute}, config.Duration{Duration: 30 * time.Minute}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = &state.Budget{Remaining: 5000, Limit: 5000, Reset: now.Add(time.Hour), At: now}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	gateOf := func(repo string) *gateRun {
		st, _ := a.store.Read()
		i := slices.IndexFunc(st.Merges, func(m state.Merge) bool { return m.Repo == repo })
		return &gateRun{app: a, ctx: context.Background(), argv: []string{merge.Tool, "release", promoteArg, repo}, repo: repo,
			lane: a.cfg.LaneOf(repo), me: st.Merges[i].By, pid: os.Getpid(), queued: true, placed: true, from: st.Merges[i].PID}
	}

	second := gateOf(platMgr)
	if why, err := second.step(); err != nil || !strings.HasPrefix(why, "position 2 in lane portal-tools behind "+backstage+"#2885") {
		t.Fatalf("the second promotion: %q, %v", why, err)
	}
	first := gateOf(repoMgr)
	for range 2 {
		why, err := first.step()
		if err != nil || why != "next in lane portal-tools behind the running "+backstage+"#2885 (\"ahead\", since "+clock(a.now, now.Add(-15*time.Minute))+")" {
			t.Fatalf("while the merge ahead runs: %q, %v", why, err)
		}
	}
	if i := indexOf(verbsOf(t, a), verbPromoting); i >= 0 {
		t.Fatalf("promoted while the merge ahead runs: %v", verbsOf(t, a))
	}

	// The merge's devctl ends: merged, its candidate cut.
	endDevctl(t, a, backstage, 2885, `{"mergeCommitSha":"abc","release":{"verdict":"available","tag":"v2.95.1-rc.1"}}`, "0")
	if why, err := first.step(); err != nil || why != "" {
		t.Fatalf("after the merge ahead settled: %q, %v", why, err)
	}
	lines := verbsOf(t, a)
	merged, promoting := indexOf(lines, verbMerged+" "+backstage+"#2885 exit 0, release v2.95.1-rc.1"), indexOf(lines, verbPromoting+" "+repoMgr)
	if merged < 0 || promoting < merged {
		t.Fatalf("the promotion did not start after the merge ahead was recorded merged (%d, %d):\n%s", merged, promoting, strings.Join(lines, "\n"))
	}
	if d := lastEventOf(t, a, verbMerged); !strings.HasPrefix(d, repoMgr+" promote exit 0, release v0.33.5") {
		t.Errorf("promoted: %q", d)
	}
}

// In a lane without an installation, a running merge whose devctl ended
// after its gate left, without a document, is judged before the promotion
// behind it starts: merged, its release unknown, the lane settles by the
// settle rule first; not merged, the promotion starts at once.
func TestAPromotionBehindAMergeWithoutItsOutcomeWaitsForItsJudgement(t *testing.T) {
	for _, c := range []struct {
		name, pull, held string
	}{
		{"merged, its release unknown", github.Merged, "next in lane o/r, o/r#889's release is unknown: the lane settles until "},
		{"nothing merged", github.Open, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			noSystemd(t)
			stubGitHub(t, c.pull, "")
			fakeDevctl(t, `echo '{"repositories":[{"candidate":"`+candidateA+`","state":"dispatched"}]}'`)
			newest := candidateA
			stubCandidate(t, &newest)
			a, g := promotePlace(t)
			a.cfg.Merge.Settle = config.Duration{Duration: 5 * time.Minute}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Merges = append([]state.Merge{{Repo: scratchRepo, PR: 889, Lane: scratchRepo, By: state.Party{Name: aheadName},
					Phase: state.Running, Joined: a.now.Add(-20 * time.Minute), Started: a.now.Add(-20 * time.Minute)}}, st.Merges...)
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			// devctl exit 7 without its document: GitHub did not answer it.
			endDevctl(t, a, scratchRepo, 889, "", "7")

			why, err := g.step()
			if err != nil || !strings.HasPrefix(why, c.held) || (c.held == "") != (why == "") {
				t.Fatalf("step: %q, %v, want %q…", why, err, c.held)
			}
			if c.held == "" {
				if d := lastEventOf(t, a, "merge.failed"); !strings.HasPrefix(d, "o/r#889 exit 7, nothing merged, it left lane o/r") {
					t.Errorf("merge.failed %q", d)
				}
				if d := lastEventOf(t, a, verbMerged); !strings.HasPrefix(d, "o/r promote exit 0") {
					t.Errorf("not promoted: %q", d)
				}
				return
			}
			if d := lastEventOf(t, a, verbMerged); !strings.HasPrefix(d, "o/r#889 exit 7, release unconfirmed (merged per GitHub)") {
				t.Errorf("merged %q", d)
			}
			st, _ := a.store.Read()
			out := a.capture(func() { a.printLanes(a.laneViews(st)) })
			if !strings.Contains(out, "o/r: settling o/r#889, its release unknown, until ") {
				t.Errorf("lanes does not say the settle:\n%s", out)
			}
			w := &watcher{app: a, last: map[string]time.Time{}}
			w.settled(context.Background(), a.now)
			if st, _ := a.store.Read(); !slices.ContainsFunc(st.Merges, func(m state.Merge) bool { return m.Phase == state.Settling }) {
				t.Fatal("the watch settled a release unknown before the settle rule")
			}

			g.now = a.now.Add(6 * time.Minute)
			if why, err := g.step(); err != nil || why != "" {
				t.Fatalf("after the settle: %q, %v", why, err)
			}
			if d := lastEventOf(t, a, verbMerged); !strings.HasPrefix(d, "o/r promote exit 0") {
				t.Errorf("not promoted after the settle: %q", d)
			}
		})
	}
}
