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

	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

func takeoverGate(t *testing.T, pid int, phase string) *gate.GateRun {
	t.Helper()
	now := time.Now()
	a := queueApp(t, state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, PID: pid,
		Phase: phase, By: state.Party{Session: "stale owner", Name: "previous worker"}, Joined: now, Seen: now, Started: now})
	return &gate.GateRun{Host: a, Ctx: context.Background(), Repo: scratchRepo, Pr: 7, Lane: a.cfg.Lanes[0],
		Me: state.Party{Session: "taking over", Name: "current worker"}, Pid: os.Getpid(), Argv: mergeArgv(scratchRepo)}
}

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestGateTakesOverDeadOwnerAndRunsMerge(t *testing.T) {
	noSystemd(t)
	stubGitHub(t, github.Open, "")
	stubBaseRelease(t, github.BaseRelease{Base: mainBranch, Auto: true}, nil)
	fakeDevctl(t, `echo '`+mergedDoc+`'`)
	g := takeoverGate(t, 1<<30, state.Running)
	setBudget(t, gateApp(g.Host), 4000)
	gateApp(g.Host).cfg.Merge.BudgetFresh.Duration = time.Hour
	if why, err := g.Step(); err != nil || why != "" {
		t.Fatalf("step: %q, %v", why, err)
	}
	if d := lastEventOf(t, gateApp(g.Host), "merge.takeover"); !strings.Contains(d, "old pid 1073741824, gate pid 1073741824 is gone") {
		t.Fatalf("takeover event: %q", d)
	}
	if d := lastEventOf(t, gateApp(g.Host), verbMerged); !strings.Contains(d, "exit 0, release v1.2.4") {
		t.Fatalf("merge outcome: %q", d)
	}
}

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestGateTakesOverMergedPRAndConfirmsRelease(t *testing.T) {
	for _, phase := range []string{state.Running, state.Waiting} {
		t.Run(phase, func(t *testing.T) {
			stubGitHub(t, github.Merged, "")
			g := takeoverGate(t, sleeper(t), phase)
			gate.PullState = func(context.Context, string, int) (github.Pull, error) {
				p := github.Pull{State: github.Merged, MergedAt: time.Now().Add(-time.Minute)}
				p.MergeCommit.OID = "merge-abc"
				return p, nil
			}
			was := gate.ReleaseWait
			t.Cleanup(func() { gate.ReleaseWait = was })
			calls := 0
			gate.ReleaseWait = func(_ context.Context, repo string, target ...string) []byte {
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
			if why, err := g.Step(); why != "" || err != nil {
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

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestGateTakesOverOlderBinaryAndLateSaveIsRefused(t *testing.T) {
	stubGitHub(t, github.Open, "")
	g := takeoverGate(t, sleeper(t), state.Running)
	old, err := state.OpenVersion(gateApp(g.Host).store.Dir(), olderRelease)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges[0].Version = olderRelease
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	gateApp(g.Host).store, err = state.OpenVersion(gateApp(g.Host).store.Dir(), newerRelease)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateApp(g.Host).store.Update(func(*state.State) ([]state.Event, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if done, err := g.Takeover(); done || err != nil {
		t.Fatalf("takeover: %v, %v", done, err)
	}
	st := gateState(t, g)
	if len(st.Merges) != 1 || st.Merges[0].PID != g.Pid || st.Merges[0].Phase != state.Waiting {
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

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestLanesAndWatchSayStaleMergeOnce(t *testing.T) {
	g := takeoverGate(t, 1<<30, state.Running)
	var out bytes.Buffer
	gateApp(g.Host).out = &out
	gateApp(g.Host).printLanes(gateApp(g.Host).laneViews(gateState(t, g)))
	if !strings.Contains(out.String(), "STALE: gate pid 1073741824 is gone") {
		t.Fatal(out.String())
	}
	out.Reset()
	w := &watcher{a: gateApp(g.Host), last: map[string]time.Time{}}
	w.staleMerges()
	w.staleMerges()
	if strings.Count(out.String(), "MERGE STALE:") != 1 {
		t.Fatal(out.String())
	}
}

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestSameSessionReusesWaitingPlace(t *testing.T) {
	stubGitHub(t, github.Open, "")
	g := takeoverGate(t, sleeper(t), state.Waiting)
	if err := gateApp(g.Host).store.Update(func(st *state.State) ([]state.Event, error) { st.Merges[0].By = g.Me; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	old := gateState(t, g).Merges[0]
	if why, err := g.Step(); why != "" || Code(err) != gate.ExitGateQueued {
		t.Fatalf("step: %q, %v", why, err)
	}
	st := gateState(t, g)
	if len(st.Merges) != 1 || st.Merges[0].PID != old.PID || !st.Merges[0].Joined.Equal(old.Joined) {
		t.Fatalf("place changed: %+v", st.Merges)
	}
}

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestMergedWaitingHeadLeavesQueueWithLiveOwner(t *testing.T) {
	stubGitHub(t, github.Merged, "")
	g := takeoverGate(t, sleeper(t), state.Waiting)
	if err := gateApp(g.Host).checkPlaces(g.Ctx, watchParty, gateApp(g.Host).now, "", "", 0, time.Second); err != nil {
		t.Fatal(err)
	}
	q := merge.Queue(gateState(t, g), scratchRepo)
	if len(q.Waiting) != 0 || q.Settling == nil || q.Settling.PR != 7 {
		t.Fatalf("merged head still queued: %+v", q)
	}
	// The original gate observes the overtaken place and returns the merged
	// outcome rather than running devctl or refusing the removed queue entry.
	g.Placed = true
	was := gate.ReleaseWait
	t.Cleanup(func() { gate.ReleaseWait = was })
	gate.ReleaseWait = func(context.Context, string, ...string) []byte {
		return []byte(`{"verdict":"available","tag":"v1.2.4"}`)
	}
	if why, err := g.Step(); why != "" || err != nil {
		t.Fatalf("overtaken gate: %q, %v", why, err)
	}
}

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestGateDoesNotTakeOverLiveOpenPR(t *testing.T) {
	stubGitHub(t, github.Open, "")
	g := takeoverGate(t, sleeper(t), state.Running)
	old := gateState(t, g).Merges[0]
	if why, err := g.Step(); why != "" || Code(err) != gate.ExitGateDuplicate {
		t.Fatalf("step: %q, %v", why, err)
	}
	if st := gateState(t, g); len(st.Merges) != 1 || st.Merges[0].PID != old.PID {
		t.Fatalf("live owner replaced: %+v", st.Merges)
	}
}

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestGateDoesNotRetryDeadOwnerWhenGitHubIsUnread(t *testing.T) {
	stubGitHub(t, "", "")
	g := takeoverGate(t, 1<<30, state.Running)
	if _, err := g.Step(); Code(err) != gate.ExitGateUnread {
		t.Fatalf("step: %v", err)
	}
	if st := gateState(t, g); len(st.Merges) != 1 || st.Merges[0].PID != 1<<30 {
		t.Fatalf("unjudged owner replaced: %+v", st.Merges)
	}
}
