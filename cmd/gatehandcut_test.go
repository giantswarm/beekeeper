//go:build unix

package cmd

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// handCutDevctl is devctl pr merge as it behaves: with --no-release-wait it
// ends at the merge (release null), without it it waits for the release,
// which on a branch without auto-release never comes.
const handCutDevctl = `case "$*" in
*--no-release-wait*) echo merged >&2; echo '{"mergeCommitSha":"abc","verdict":"merged","release":null}' ;;
*) echo "waiting for the release" >&2; exec sleep 30 ;;
esac`

// mainBranch is a base branch Auto-release tags.
const mainBranch = "main"

// stubBaseRelease answers the base branch's release model and counts the
// reads.
func stubBaseRelease(t *testing.T, r github.BaseRelease, err error) *int {
	t.Helper()
	asked := new(int)
	was := baseRelease
	t.Cleanup(func() { baseRelease = was })
	baseRelease = func(context.Context, string, int) (github.BaseRelease, error) {
		*asked++
		return r, err
	}
	return asked
}

// startMerge makes runningMerge's merge wait for its turn and starts it.
func startMerge(t *testing.T, g *gateRun) error {
	t.Helper()
	why, err := turnCame(t, g)
	if why != "" {
		t.Fatalf("the merge did not start: %s", why)
	}
	return err
}

// turnCame makes runningMerge's merge wait for its turn, under a devctl cap
// of 10, and starts it: the reason it still waits, if any.
func turnCame(t *testing.T, g *gateRun) (string, error) {
	t.Helper()
	g.cfg.Merge.Cap = 10
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges[0].Phase, st.Merges[0].Started = state.Waiting, time.Time{}
		return nil, nil
	})
	if err := g.readRelease(); err != nil {
		return "", err
	}
	return g.start("", nil)
}

// The devctl cap counts the machine's devctl processes from its process
// table: a merge whose turn came waits while the cap's number run.
func TestAMergeWhoseTurnCameWaitsAtTheMachinesDevctlCap(t *testing.T) {
	stubBaseRelease(t, github.BaseRelease{Base: mainBranch, Auto: true}, nil)
	g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}})
	hostDevctls(t, 10)
	why, err := turnCame(t, g)
	if err != nil || !strings.Contains(why, "10 devctl processes run machine-wide (cap 10)") {
		t.Fatalf("start: %q, %v; want a wait for the cap", why, err)
	}
	if st := gateState(t, g); st.Merges[0].Phase != state.Waiting {
		t.Errorf("the merge started: %+v", st.Merges)
	}
}

// A merge into a branch no Auto-release run tags (a fork line's maintenance
// branch, its tags cut by hand) ends at the merge and frees its lane at
// once, though the lane has an installation to roll; a merge into a branch
// with auto-release still settles its lane until the release rolled.
func TestAMergeIntoAHandCutBranchFreesItsLaneAtTheMerge(t *testing.T) {
	lane := config.Lane{Name: serving, Installation: gazelle, Repositories: []string{scratchRepo}}
	t.Run("no auto-release", func(t *testing.T) {
		noSystemd(t)
		fakeDevctl(t, handCutDevctl)
		stubGitHub(t, github.Merged, "")
		asked := stubBaseRelease(t, github.BaseRelease{Base: "release-1.3"}, nil)
		g := runningMerge(t, scratchRepo, lane)
		done := time.Now()
		if err := startMerge(t, g); err != nil {
			t.Fatalf("exit %d, want 0", Code(err))
		}
		if time.Since(done) > 10*time.Second {
			t.Errorf("the merge waited %s for a release", time.Since(done))
		}
		if st := gateState(t, g); len(st.Merges) != 0 {
			t.Errorf("the lane is not free: %+v", st.Merges)
		}
		if d := lastEvent(t, g, verbMerged); !strings.Contains(d, "o/r#7 exit 0, release none awaited (release-1.3 has no auto-release)") {
			t.Errorf("merged event: %q", d)
		}
		if g.readRelease() != nil || *asked != 1 {
			t.Errorf("the base branch was read %d times, want once per merge", *asked)
		}
	})
	t.Run("auto-release", func(t *testing.T) {
		noSystemd(t)
		fakeDevctl(t, `case "$*" in *--no-release-wait*) exit 7 ;; esac; echo '`+mergedDoc+`'`)
		stubGitHub(t, github.Merged, "")
		stubBaseRelease(t, github.BaseRelease{Base: mainBranch, Auto: true}, nil)
		g := runningMerge(t, scratchRepo, lane)
		if err := startMerge(t, g); err != nil {
			t.Fatalf("exit %d, want 0", Code(err))
		}
		st := gateState(t, g)
		if len(st.Merges) != 1 || st.Merges[0].Phase != state.Settling || st.Merges[0].Release != "v1.2.4" || st.Merges[0].HandCut != "" {
			t.Errorf("want the merge settling on v1.2.4: %+v", st.Merges)
		}
	})
}

// GitHub not answering for the base branch, its last try too, refuses the
// merge before devctl runs; a repository on the plain squash merge, which awaits no release, is
// not asked.
func TestTheBaseBranchUnreadRefusesTheMerge(t *testing.T) {
	asked := stubBaseRelease(t, github.BaseRelease{}, errors.New("connection reset"))
	g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo})
	if err := g.readRelease(); Code(err) != ExitGateRefused {
		t.Fatalf("exit %d, want %d", Code(err), ExitGateRefused)
	}
	if d := lastEvent(t, g, "merge.refused"); !strings.Contains(d, "connection reset") {
		t.Errorf("merge.refused event: %q", d)
	}
	g = runningMerge(t, labRepo, config.Lane{Name: labRepo})
	if err := g.readRelease(); err != nil || *asked != 1+len(readBackoff) {
		t.Errorf("the squash route: %v, %d reads", err, *asked)
	}
}

// A SIGTERM aimed at the gate, its caller still there, stops devctl too:
// the run ends as one ended by a signal and GitHub judges it.
func TestASIGTERMToTheGateStopsItsDevctl(t *testing.T) {
	noSystemd(t)
	fakeDevctl(t, `echo "waiting for the release" >&2; exec sleep 30`)
	stubGitHub(t, github.Merged, "")
	g := runningMerge(t, scratchRepo, config.Lane{Name: serving, Installation: gazelle, Repositories: []string{scratchRepo}})
	go func() {
		time.Sleep(600 * time.Millisecond)
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	}()
	done := time.Now()
	if err := g.runMerge(); Code(err) != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit %d, want 143", Code(err))
	}
	if time.Since(done) > 10*time.Second {
		t.Errorf("devctl ran on for %s after the SIGTERM", time.Since(done))
	}
	if d := lastEvent(t, g, verbMerged); !strings.Contains(d, "exit 143, release unconfirmed") {
		t.Errorf("merged event: %q", d)
	}
}

// stubPluginTags answers the tags a merge's per-plugin Auto Release cuts and
// whether each exists yet: a tag appears at its second read.
func stubPluginTags(t *testing.T, tags []string) {
	t.Helper()
	wasTags, wasExists, wasPoll := pluginTags, tagExists, pluginTagPoll
	t.Cleanup(func() { pluginTags, tagExists, pluginTagPoll = wasTags, wasExists, wasPoll })
	pluginTagPoll = 10 * time.Millisecond
	pluginTags = func(_ context.Context, repo, sha string) ([]string, error) {
		if repo != scratchRepo || sha != "abc" {
			t.Errorf("tags of %s@%s asked, want %s@abc", repo, sha, scratchRepo)
		}
		return tags, nil
	}
	reads := map[string]int{}
	tagExists = func(_ context.Context, _, tag string) (bool, error) {
		reads[tag]++
		return reads[tag] > 1, nil
	}
}

// A merge into a branch whose Auto Release tags per plugin (a plugin
// marketplace, <plugin>/vX.Y.Z) ends devctl at the merge and awaits the tag
// of the plugin whose version it changed, which is its release; one that
// changed no version warrants none. A repository-wide Auto-release is
// devctl's to await, as before.
func TestAMergeIntoAPerPluginRepositoryAwaitsThePluginsTag(t *testing.T) {
	lane := config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}}
	plugins := github.BaseRelease{Base: mainBranch, Plugins: true}
	t.Run("a plugin's version changed", func(t *testing.T) {
		noSystemd(t)
		fakeDevctl(t, handCutDevctl)
		stubGitHub(t, github.Merged, "")
		stubBaseRelease(t, plugins, nil)
		stubPluginTags(t, []string{"gs-plans/v0.3.0"})
		g := runningMerge(t, scratchRepo, lane)
		if err := startMerge(t, g); err != nil {
			t.Fatalf("exit %d, want 0", Code(err))
		}
		if st := gateState(t, g); len(st.Merges) != 0 {
			t.Errorf("the lane is not free: %+v", st.Merges)
		}
		if d := lastEvent(t, g, verbMerged); !strings.Contains(d, "release gs-plans/v0.3.0") {
			t.Errorf("merged event: %q, want the plugin's tag as its release", d)
		}
	})
	t.Run("no plugin's version changed", func(t *testing.T) {
		noSystemd(t)
		fakeDevctl(t, handCutDevctl)
		stubGitHub(t, github.Merged, "")
		stubBaseRelease(t, plugins, nil)
		stubPluginTags(t, nil)
		g := runningMerge(t, scratchRepo, lane)
		if err := startMerge(t, g); err != nil {
			t.Fatalf("exit %d, want 0", Code(err))
		}
		if d := lastEvent(t, g, verbMerged); !strings.Contains(d, "release none warranted") {
			t.Errorf("merged event: %q", d)
		}
	})
	t.Run("a repository-wide auto-release", func(t *testing.T) {
		noSystemd(t)
		fakeDevctl(t, `case "$*" in *--no-release-wait*) exit 7 ;; esac; echo '`+mergedDoc+`'`)
		stubGitHub(t, github.Merged, "")
		stubBaseRelease(t, github.BaseRelease{Base: mainBranch, Auto: true}, nil)
		stubPluginTags(t, []string{"never/v1.0.0"})
		pluginTags = func(context.Context, string, string) ([]string, error) {
			t.Error("plugin tags asked of a repository-wide auto-release")
			return nil, nil
		}
		g := runningMerge(t, scratchRepo, lane)
		if err := startMerge(t, g); err != nil {
			t.Fatalf("exit %d, want 0", Code(err))
		}
		if d := lastEvent(t, g, verbMerged); !strings.Contains(d, "release v1.2.4") {
			t.Errorf("merged event: %q", d)
		}
	})
}

// releasePRTag is the tag a release pull request's merge cuts.
const releasePRTag = "v1.4.0"

// stubReleaseWait answers devctl release wait with doc and records the
// version asked.
func stubReleaseWait(t *testing.T, doc string) *[]string {
	t.Helper()
	asked := new([]string)
	was := releaseWait
	t.Cleanup(func() { releaseWait = was })
	releaseWait = func(_ context.Context, repo, version string) []byte {
		*asked = append(*asked, repo+" "+version)
		return []byte(doc)
	}
	return asked
}

// A merge into a branch that releases through the release-PR flow (the bot's
// <base>#release#<level> pull request, tagged on its merge) names that flow,
// not a hand cut: the release pull request's merge awaits its tag pipeline
// and settles its lane on the release, any other merge cuts no tag and frees
// its lane. A branch with neither flow stays cut by hand
// (TestAMergeIntoAHandCutBranchFreesItsLaneAtTheMerge).
func TestAMergeIntoAReleasePRBranchTracksTheReleasePullRequestsTag(t *testing.T) {
	lane := config.Lane{Name: serving, Installation: gazelle, Repositories: []string{scratchRepo}}
	t.Run("the release pull request", func(t *testing.T) {
		noSystemd(t)
		fakeDevctl(t, handCutDevctl)
		stubGitHub(t, github.Merged, "")
		stubBaseRelease(t, github.BaseRelease{Base: mainBranch, ReleasePR: true, Version: releasePRTag}, nil)
		asked := stubReleaseWait(t, `{"verdict":"available","reason":"","tag":"v1.4.0"}`)
		g := runningMerge(t, scratchRepo, lane)
		var err error
		out := gateStderr(t, func() { err = startMerge(t, g) })
		if err != nil {
			t.Fatalf("exit %d, want 0", Code(err))
		}
		if len(*asked) != 1 || (*asked)[0] != scratchRepo+" "+releasePRTag {
			t.Errorf("release waits: %q, want one of %s v1.4.0", *asked, scratchRepo)
		}
		if !strings.Contains(out, "is the release pull request of v1.4.0: awaiting its tag pipeline") || strings.Contains(out, "no Auto-release run tags") {
			t.Errorf("gate lines:\n%s", out)
		}
		st := gateState(t, g)
		if len(st.Merges) != 1 || st.Merges[0].Phase != state.Settling || st.Merges[0].Release != releasePRTag || st.Merges[0].HandCut != "" {
			t.Errorf("want the merge settling on v1.4.0: %+v", st.Merges)
		}
	})
	t.Run("the release pull request's tag pipeline failed", func(t *testing.T) {
		noSystemd(t)
		fakeDevctl(t, handCutDevctl)
		stubGitHub(t, github.Merged, "")
		stubBaseRelease(t, github.BaseRelease{Base: mainBranch, ReleasePR: true, Version: releasePRTag}, nil)
		stubReleaseWait(t, `{"verdict":"ci_failed","reason":"push-to-registries failed","tag":"v1.4.0"}`)
		g := runningMerge(t, scratchRepo, lane)
		var err error
		out := gateStderr(t, func() { err = startMerge(t, g) })
		if err != nil {
			t.Fatalf("exit %d, want 0", Code(err))
		}
		if !strings.Contains(out, "its release v1.4.0 failed (push-to-registries failed)") {
			t.Errorf("gate lines:\n%s", out)
		}
		if d := lastEvent(t, g, verbMerged); !strings.Contains(d, "release v1.4.0 failed, nothing rolls") {
			t.Errorf("merged event: %q", d)
		}
	})
	t.Run("another pull request", func(t *testing.T) {
		noSystemd(t)
		fakeDevctl(t, handCutDevctl)
		stubGitHub(t, github.Merged, "")
		stubBaseRelease(t, github.BaseRelease{Base: mainBranch, ReleasePR: true}, nil)
		asked := stubReleaseWait(t, "")
		g := runningMerge(t, scratchRepo, lane)
		var err error
		out := gateStderr(t, func() { err = startMerge(t, g) })
		if err != nil {
			t.Fatalf("exit %d, want 0", Code(err))
		}
		if len(*asked) != 0 {
			t.Errorf("release waits: %q, want none", *asked)
		}
		if !strings.Contains(out, "which releases through the release-PR flow (the bot opens a main#release#<level> pull request, zz_generated.create_release.yaml tags its merge)") ||
			strings.Contains(out, "no Auto-release run tags") || strings.Contains(out, "cut by hand") {
			t.Errorf("gate lines:\n%s", out)
		}
		if st := gateState(t, g); len(st.Merges) != 0 {
			t.Errorf("the lane is not free: %+v", st.Merges)
		}
		if d := lastEvent(t, g, verbMerged); !strings.Contains(d, "release none warranted") {
			t.Errorf("merged event: %q", d)
		}
	})
}
