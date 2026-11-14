package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The repositories and lane of the hung-merge tests.
const (
	hungRepo, openRepo, freshRepo, nextRepo = "o/hung", "o/open", "o/fresh", "o/next"
	hungLane, openLane                      = "scratch", "open"
)

// hungChild starts a process standing in for a merge's merge-child, records
// it as repo#pr's in the state directory and returns its pid; it is reaped
// once it ends.
func hungChild(t *testing.T, dir, repo string, pr int) int {
	t.Helper()
	c := exec.Command("sleep", "60")
	if err := c.Start(); err != nil {
		t.Skip("no sleep:", err)
	}
	t.Cleanup(func() { _ = c.Process.Kill() })
	go func() { _ = c.Wait() }()
	base := gate.MergeBase(dir, repo, pr)
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".pid", []byte(strconv.Itoa(c.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	return c.Process.Pid
}

// stubPulls answers GitHub with each pull request's state by number.
func stubPulls(t *testing.T, pulls map[int]github.Pull) {
	t.Helper()
	was, wait := gate.PullState, gate.JudgeWait
	t.Cleanup(func() { gate.PullState, gate.JudgeWait = was, wait })
	gate.JudgeWait = 0
	gate.PullState = func(_ context.Context, _ string, n int) (github.Pull, error) {
		if p, ok := pulls[n]; ok {
			return p, nil
		}
		return github.Pull{}, errors.New("no network")
	}
}

// A merge whose devctl runs on merge.hungAfter after its pull request
// merged is ended by the watch, and the next poll records it: its release
// unconfirmed, it settles its lane by the settle rule. A run whose pull
// request is open, or that merged recently, runs on.
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestWatchEndsAHungMerge(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	watchApp(w.Host).now = relayNow
	hungAfter := watchApp(w.Host).cfg.Merge.HungAfter.Duration
	// The watch has been observing continuously for a full grace period.
	w.LastHungCheck = relayNow.Add(-time.Second)
	w.HungGrace = relayNow.Add(-hungAfter - time.Minute)
	dir := watchApp(w.Host).store.Dir()
	stubPulls(t, map[int]github.Pull{
		1: {State: github.Merged, MergedAt: relayNow.Add(-hungAfter - time.Minute)},
		2: {State: github.Open},
		3: {State: github.Merged, MergedAt: relayNow.Add(-time.Minute)},
	})
	hung, open, fresh := hungChild(t, dir, hungRepo, 1), hungChild(t, dir, openRepo, 2), hungChild(t, dir, freshRepo, 3)
	long := relayNow.Add(-2 * hungAfter)
	err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{
			// Its gate was killed with its caller; devctl runs on.
			{Repo: hungRepo, PR: 1, Lane: hungLane, By: four, Child: hung, Phase: state.Running, Started: long},
			{Repo: nextRepo, PR: 4, Lane: hungLane, By: four, Phase: state.Waiting, Joined: relayNow, Seen: relayNow},
			{Repo: openRepo, PR: 2, Lane: openLane, By: four, Child: open, Phase: state.Running, Started: long},
			{Repo: freshRepo, PR: 3, Lane: testFresh, By: four, Child: fresh, Phase: state.Running, Started: long},
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w.HungMerges(context.Background(), watchApp(w.Host).now)
	if l := out.String(); strings.Count(l, "MERGE HUNG") != 1 || !strings.Contains(l, `MERGE HUNG: o/hung#1 in lane scratch by "Agent four": merged at`) {
		t.Fatalf("watch lines:\n%s", l)
	}
	if !eventually(5*time.Second, func() bool { return !proc.Alive(hung) }) {
		t.Fatal("the hung devctl runs on")
	}
	if !proc.Alive(open) || !proc.Alive(fresh) {
		t.Error("a run that has not outlived its pull request was ended")
	}
	w.LostMerges(context.Background())
	if l := out.String(); !strings.Contains(l, "MERGE RECORDED: o/hung#1 exit 137, release unconfirmed (merged per GitHub)") {
		t.Fatalf("watch lines:\n%s", l)
	}
	st, err := watchApp(w.Host).store.Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range st.Merges {
		want := map[string]string{hungRepo: state.Settling, nextRepo: state.Waiting, openRepo: state.Running, freshRepo: state.Running}[m.Repo]
		if m.Phase != want {
			t.Errorf("%s is %s, want %s", m.Key(), m.Phase, want)
		}
	}
	if evs, err := watchApp(w.Host).store.Events(10, func(e state.Event) bool { return e.Verb == "merge.hung" }); err != nil || len(evs) != 1 {
		t.Errorf("merge.hung events: %d, %v", len(evs), err)
	}
}

// A wall-clock jump representing suspend must not turn a live release
// wait into a hung merge. Continuous awake checks still end it afterwards.
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestWatchHungMergeAcrossSuspend(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	watchApp(w.Host).now = relayNow
	after := watchApp(w.Host).cfg.Merge.HungAfter.Duration
	child := hungChild(t, watchApp(w.Host).store.Dir(), hungRepo, 1)
	stubPulls(t, map[int]github.Pull{
		1: {State: github.Merged, MergedAt: relayNow.Add(-after - time.Minute)},
	})
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{{Repo: hungRepo, PR: 1, Lane: hungLane, By: four,
			Child: child, Phase: state.Running, Started: relayNow.Add(-time.Hour)}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	w.HungMerges(ctx, relayNow)
	if !proc.Alive(child) || strings.Contains(out.String(), "MERGE HUNG") {
		t.Fatalf("wait ended on startup without awake observations: %s", out)
	}
	// Three hours pass on the wall clock with no watch ticks.
	resumed := relayNow.Add(3 * time.Hour)
	w.HungMerges(ctx, resumed)
	if !proc.Alive(child) || strings.Contains(out.String(), "MERGE HUNG") {
		t.Fatalf("wait ended after suspend: %s", out)
	}
	// Normal ticks accumulate a full awake grace period, including its
	// boundary, without relying on real sleeps in the test.
	for elapsed := time.Minute; elapsed <= after; elapsed += time.Minute {
		w.HungMerges(ctx, resumed.Add(elapsed))
	}
	if !proc.Alive(child) || strings.Contains(out.String(), "MERGE HUNG") {
		t.Fatalf("wait ended before awake threshold: %s", out)
	}
	w.HungMerges(ctx, resumed.Add(after+time.Second))
	if !eventually(5*time.Second, func() bool { return !proc.Alive(child) }) {
		t.Fatal("silent wait survived the awake threshold")
	}
	if strings.Count(out.String(), "MERGE HUNG") != 1 {
		t.Fatalf("watch lines: %s", out)
	}
}

// lanes drop ends a running merge whose pull request merged while its
// devctl runs on, and records it; one whose pull request is open is refused.
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestLanesDropEndsAHungRun(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	a := watchApp(w.Host)
	a.as, a.now = "Agent four", time.Now()
	dir := a.store.Dir()
	stubPulls(t, map[int]github.Pull{
		1: {State: github.Merged, MergedAt: a.now.Add(-time.Minute)},
		2: {State: github.Open},
	})
	hung, open := hungChild(t, dir, hungRepo, 1), hungChild(t, dir, openRepo, 2)
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{
			{Repo: hungRepo, PR: 1, Lane: hungLane, By: four, Child: hung, Phase: state.Running, Started: a.now},
			{Repo: openRepo, PR: 2, Lane: openLane, By: four, Child: open, Phase: state.Running, Started: a.now},
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	drop := func(n string) error {
		c := a.lanesCmd()
		c.SetArgs([]string{"drop", map[string]string{"1": hungRepo, "2": openRepo}[n], n})
		c.SetOut(out)
		return c.Execute()
	}
	if err := drop("1"); err != nil {
		t.Fatal(err)
	}
	if l := out.String(); !strings.Contains(l, "ended the devctl of o/hung#1 (merged at") || !strings.Contains(l, "settling o/hung#1, its release unknown, until ") {
		t.Fatalf("drop said:\n%s", l)
	}
	if err := drop("2"); err == nil || !strings.Contains(err.Error(), "its pull request is open") {
		t.Errorf("drop of a run whose pull request is open: %v", err)
	}
	if !proc.Alive(open) {
		t.Error("the open run's devctl was ended")
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	phases := map[string]string{}
	for _, m := range st.Merges {
		phases[m.Repo] = m.Phase
	}
	if len(st.Merges) != 2 || phases[hungRepo] != state.Settling || phases[openRepo] != state.Running {
		t.Errorf("merges: %+v", st.Merges)
	}
}

// A long post-merge CircleCI wait stays alive while devctl reports progress,
// even after the watch's historical hung threshold has elapsed.
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestWatchKeepsActiveReleasePipeline(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	now := relayNow
	after := watchApp(w.Host).cfg.Merge.HungAfter.Duration
	started := now.Add(-3 * after)
	// Model continuous watch observations, without a suspend grace reset.
	w.HungGrace = started
	w.LastHungCheck = now.Add(-time.Second)
	stubPulls(t, map[int]github.Pull{1: {State: github.Merged, MergedAt: started}})
	child := hungChild(t, watchApp(w.Host).store.Dir(), hungRepo, 1)
	base := gate.MergeBase(watchApp(w.Host).store.Dir(), hungRepo, 1)
	p := newMergeProgress(base, []string{"--release-timeout=30m"}, started)
	p.observe("release: waiting for the release of abc\n", started.Add(time.Minute))
	p.observe("pipeline 123: workflow build running\n", now.Add(-time.Minute))
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{{Repo: hungRepo, PR: 1, Lane: hungLane, Child: child, Phase: state.Running, Started: started}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	w.HungMerges(context.Background(), now)
	if !proc.Alive(child) || strings.Contains(out.String(), "MERGE HUNG") {
		t.Fatalf("active release ended: %s", out.String())
	}
	// Once devctl has been silent for its release timeout, the same watch
	// still recovers a stuck child.
	w.LastHungCheck = now.Add(30*time.Minute - time.Second)
	w.HungMerges(context.Background(), now.Add(30*time.Minute))
	if !strings.Contains(out.String(), "MERGE HUNG") {
		t.Fatalf("silent release not ended: %s", out.String())
	}
}
