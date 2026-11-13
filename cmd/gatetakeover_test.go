//go:build unix

package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

func takeoverGate(t *testing.T, pid int, phase string) *gateRun {
	t.Helper()
	now := time.Now()
	a := queueApp(t, state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, PID: pid,
		Phase: phase, By: state.Party{Session: "stale owner", Name: "previous worker"}, Joined: now, Seen: now, Started: now})
	return &gateRun{a: a, ctx: context.Background(), repo: scratchRepo, pr: 7, lane: a.cfg.Lanes[0],
		me: state.Party{Session: "taking over", Name: "current worker"}, pid: os.Getpid(), argv: mergeArgv(scratchRepo)}
}

func TestGateTakesOverDeadOwnerAndRunsMerge(t *testing.T) {
	noSystemd(t)
	stubGitHub(t, github.Open, "")
	stubBaseRelease(t, github.BaseRelease{Base: mainBranch, Auto: true}, nil)
	fakeDevctl(t, `echo '`+mergedDoc+`'`)
	g := takeoverGate(t, 1<<30, state.Running)
	setBudget(t, g.a, 4000)
	g.a.cfg.Merge.BudgetFresh.Duration = time.Hour
	if why, err := g.step(); err != nil || why != "" {
		t.Fatalf("step: %q, %v", why, err)
	}
	if d := lastEventOf(t, g.a, "merge.takeover"); !strings.Contains(d, "old pid 1073741824, gate pid 1073741824 is gone") {
		t.Fatalf("takeover event: %q", d)
	}
	if d := lastEventOf(t, g.a, verbMerged); !strings.Contains(d, "exit 0, release v1.2.4") {
		t.Fatalf("merge outcome: %q", d)
	}
}

func TestGateTakesOverMergedPRAndConfirmsRelease(t *testing.T) {
	for _, phase := range []string{state.Running, state.Waiting} {
		t.Run(phase, func(t *testing.T) {
			stubGitHub(t, github.Merged, "")
			g := takeoverGate(t, sleeper(t), phase)
			pullState = func(context.Context, string, int) (github.Pull, error) {
				p := github.Pull{State: github.Merged, MergedAt: time.Now().Add(-time.Minute)}
				p.MergeCommit.OID = "merge-abc"
				return p, nil
			}
			was := releaseWait
			t.Cleanup(func() { releaseWait = was })
			calls := 0
			releaseWait = func(_ context.Context, repo string, target ...string) []byte {
				calls++
				q := merge.Queue(gateState(t, g), scratchRepo)
				if q.Running != nil || q.Settling == nil || !q.Settling.Releasing {
					t.Fatalf("merged outcome not recorded before release wait: %+v", q)
				}
				if repo != scratchRepo || strings.Join(target, " ") != "--pr 7" {
					t.Fatalf("release wait: %s %v", repo, target)
				}
				return []byte(`{"verdict":"available","tag":"v1.2.6"}`)
			}
			stdout := gateStdout(t)
			if why, err := g.step(); why != "" || err != nil {
				t.Fatalf("step: %q, %v", why, err)
			}
			out, ok := merge.ParseDocument([]byte(stdout()))
			if !ok || !out.Merged || out.Release != "v1.2.6" || calls != 1 {
				t.Fatalf("outcome %+v, parsed %v, waits %d", out, ok, calls)
			}
			if st := gateState(t, g); len(st.Merges) != 0 {
				t.Fatalf("merged PR still occupies lane: %+v", st.Merges)
			}
		})
	}
}

func TestGateTakesOverOlderBinaryAndLateSaveIsRefused(t *testing.T) {
	stubGitHub(t, github.Open, "")
	g := takeoverGate(t, sleeper(t), state.Running)
	old, err := state.OpenVersion(g.a.store.Dir(), olderRelease)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges[0].Version = olderRelease
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	g.a.store, err = state.OpenVersion(g.a.store.Dir(), newerRelease)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.a.store.Update(func(*state.State) ([]state.Event, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if done, err := g.takeover(); done || err != nil {
		t.Fatalf("takeover: %v, %v", done, err)
	}
	st := gateState(t, g)
	if len(st.Merges) != 1 || st.Merges[0].PID != g.pid || st.Merges[0].Phase != state.Waiting {
		t.Fatalf("place not taken over: %+v", st.Merges)
	}
	err = old.Update(func(st *state.State) ([]state.Event, error) { st.Merges = nil; return nil, nil })
	var stale *state.StaleWriterError
	if !errors.As(err, &stale) {
		t.Fatalf("late save: %v", err)
	}
	if len(gateState(t, g).Merges) != 1 {
		t.Fatal("late save removed the replacement place")
	}
}

func TestLanesAndWatchSayStaleMergeOnce(t *testing.T) {
	g := takeoverGate(t, 1<<30, state.Running)
	var out bytes.Buffer
	g.a.out = &out
	g.a.printLanes(g.a.laneViews(gateState(t, g)))
	if !strings.Contains(out.String(), "STALE: gate pid 1073741824 is gone") {
		t.Fatal(out.String())
	}
	out.Reset()
	w := &watcher{app: g.a, last: map[string]time.Time{}}
	w.staleMerges()
	w.staleMerges()
	if strings.Count(out.String(), "MERGE STALE:") != 1 {
		t.Fatal(out.String())
	}
}

func TestSameSessionReusesWaitingPlace(t *testing.T) {
	stubGitHub(t, github.Open, "")
	g := takeoverGate(t, sleeper(t), state.Waiting)
	if err := g.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Merges[0].By = g.me; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	old := gateState(t, g).Merges[0]
	if why, err := g.step(); why != "" || Code(err) != ExitGateQueued {
		t.Fatalf("step: %q, %v", why, err)
	}
	st := gateState(t, g)
	if len(st.Merges) != 1 || st.Merges[0].PID != old.PID || !st.Merges[0].Joined.Equal(old.Joined) {
		t.Fatalf("place changed: %+v", st.Merges)
	}
}

func TestMergedWaitingHeadLeavesQueueWithLiveOwner(t *testing.T) {
	stubGitHub(t, github.Merged, "")
	g := takeoverGate(t, sleeper(t), state.Waiting)
	if err := g.a.checkPlaces(g.ctx, watchParty, g.a.now, "", "", 0, time.Second); err != nil {
		t.Fatal(err)
	}
	q := merge.Queue(gateState(t, g), scratchRepo)
	if len(q.Waiting) != 0 || q.Settling == nil || q.Settling.PR != 7 {
		t.Fatalf("merged head still queued: %+v", q)
	}
	// The original gate observes the overtaken place and returns the merged
	// outcome rather than running devctl or refusing the removed queue entry.
	g.placed = true
	was := releaseWait
	t.Cleanup(func() { releaseWait = was })
	releaseWait = func(context.Context, string, ...string) []byte {
		return []byte(`{"verdict":"available","tag":"v1.2.4"}`)
	}
	if why, err := g.step(); why != "" || err != nil {
		t.Fatalf("overtaken gate: %q, %v", why, err)
	}
}

func TestGateDoesNotTakeOverLiveOpenPR(t *testing.T) {
	stubGitHub(t, github.Open, "")
	g := takeoverGate(t, sleeper(t), state.Running)
	old := gateState(t, g).Merges[0]
	if why, err := g.step(); why != "" || Code(err) != ExitGateDuplicate {
		t.Fatalf("step: %q, %v", why, err)
	}
	if st := gateState(t, g); len(st.Merges) != 1 || st.Merges[0].PID != old.PID {
		t.Fatalf("live owner replaced: %+v", st.Merges)
	}
}

func TestGateDoesNotRetryDeadOwnerWhenGitHubIsUnread(t *testing.T) {
	stubGitHub(t, "", "")
	g := takeoverGate(t, 1<<30, state.Running)
	if _, err := g.step(); Code(err) != ExitGateUnread {
		t.Fatalf("step: %v", err)
	}
	if st := gateState(t, g); len(st.Merges) != 1 || st.Merges[0].PID != 1<<30 {
		t.Fatalf("unjudged owner replaced: %+v", st.Merges)
	}
}
