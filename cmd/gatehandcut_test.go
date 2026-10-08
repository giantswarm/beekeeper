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
	g.cfg.Merge.Cap = 10
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges[0].Phase, st.Merges[0].Started = state.Waiting, time.Time{}
		return nil, nil
	})
	if err := g.readRelease(); err != nil {
		return err
	}
	why, err := g.start("", nil)
	if why != "" {
		t.Fatalf("the merge did not start: %s", why)
	}
	return err
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
		stubBaseRelease(t, github.BaseRelease{Base: "main", Auto: true}, nil)
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

// GitHub not answering for the base branch refuses the merge before devctl
// runs; a repository on the plain squash merge, which awaits no release, is
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
	if err := g.readRelease(); err != nil || *asked != 1 {
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
