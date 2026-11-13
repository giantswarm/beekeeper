//go:build unix

package cmd

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestSignalMergeRecoversReleaseBeforeSettling(t *testing.T) {
	noSystemd(t)
	fakeDevctl(t, `kill -TERM $$`)
	stubGitHub(t, github.Merged, "")
	lane := config.Lane{Name: serving, Installation: gazelle, Repositories: []string{scratchRepo}}
	g := runningMerge(t, scratchRepo, lane)
	was := ReleaseWait
	t.Cleanup(func() { ReleaseWait = was })
	ReleaseWait = func(ctx context.Context, repo string, target ...string) []byte {
		if repo != scratchRepo || strings.Join(target, " ") != "--pr 7" {
			t.Fatalf("lookup: %s %v", repo, target)
		}
		st := gateState(t, g)
		if len(st.Merges) != 1 || st.Merges[0].Phase != state.Running {
			t.Fatalf("entered settling before lookup finished: %+v", st.Merges)
		}
		// devctl resolves the merged PR's commit to its release tag.
		return []byte(`{"verdict":"available","tag":"v1.2.4-rc.1"}`)
	}
	if err := g.RunMerge(); Code(err) != 143 {
		t.Fatalf("exit: %v", err)
	}
	st := gateState(t, g)
	if len(st.Merges) != 1 || st.Merges[0].Phase != state.Settling || st.Merges[0].Release != "v1.2.4-rc.1" {
		t.Fatalf("release not recovered: %+v", st.Merges)
	}
}

func TestNameSettlingReleaseUsesNormalRollRules(t *testing.T) {
	for _, version := range []string{"v1.2.5", "v1.2.5-rc.1"} {
		t.Run(version, func(t *testing.T) {
			lane := config.Lane{Name: serving, Installation: gazelle, Repositories: []string{scratchRepo}}
			g := runningMerge(t, scratchRepo, lane)
			g.Host.out = io.Discard
			now := time.Now()
			err := g.Host.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Merges[0].Phase, st.Merges[0].Finished = state.Settling, now
				st.Merges[0].Outside = true
				st.Merges[0].Roll = []string{"ns/chart"}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			g.Host.as = litOperator
			c := g.Host.lanesCmd()
			c.SetArgs([]string{"settle", scratchRepo, "7", "--release", version})
			if err := c.Execute(); err != nil {
				t.Fatal(err)
			}
			m := gateState(t, g).Merges[0]
			if m.Release != version || m.Outside {
				t.Fatalf("not release tracked: %+v", m)
			}
			hrs := []merge.HelmRelease{{Key: "ns/chart", Chart: "r", Ready: true, Version: "1.2.2", Range: ">=1.0.0"}}
			ready, _ := merge.Ready(lane, hrs, &m, now, time.Hour)
			if ready != merge.PreRelease(version) {
				t.Fatalf("before roll ready=%v for %s", ready, version)
			}
			hrs[0].Version = "1.2.5"
			if ready, why := merge.Ready(lane, hrs, &m, now, time.Hour); !ready {
				t.Fatalf("rolled/unadmitted release did not settle: %s", why)
			}
			if err := g.Host.nameSettlingRelease(g.Me, scratchRepo, 7, "v9.0.0"); Code(err) != ExitRefused {
				t.Fatalf("known release replaced: %v", err)
			}
		})
	}
}

func TestNameReleaseRefusesRunningAndMissingMerges(t *testing.T) {
	g := runningMerge(t, scratchRepo, localLane)
	g.Host.out = io.Discard
	for _, pr := range []int{7, 8} {
		if err := g.Host.nameSettlingRelease(g.Me, scratchRepo, pr, "v1.2.6"); Code(err) != ExitRefused {
			t.Fatalf("named a non-settling merge: %v", err)
		}
	}
}
