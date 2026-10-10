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
	base := mergeBase(dir, repo, pr)
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
	was, wait := pullState, judgeWait
	t.Cleanup(func() { pullState, judgeWait = was, wait })
	judgeWait = 0
	pullState = func(_ context.Context, _ string, n int) (github.Pull, error) {
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
func TestWatchEndsAHungMerge(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	w.now = relayNow
	hungAfter := w.cfg.Merge.HungAfter.Duration
	dir := w.store.Dir()
	stubPulls(t, map[int]github.Pull{
		1: {State: github.Merged, MergedAt: relayNow.Add(-hungAfter - time.Minute)},
		2: {State: github.Open},
		3: {State: github.Merged, MergedAt: relayNow.Add(-time.Minute)},
	})
	hung, open, fresh := hungChild(t, dir, hungRepo, 1), hungChild(t, dir, openRepo, 2), hungChild(t, dir, freshRepo, 3)
	long := relayNow.Add(-2 * hungAfter)
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{
			// Its gate was killed with its caller; devctl runs on.
			{Repo: hungRepo, PR: 1, Lane: hungLane, By: four, Child: hung, Phase: state.Running, Started: long},
			{Repo: nextRepo, PR: 4, Lane: hungLane, By: four, Phase: state.Waiting, Joined: relayNow, Seen: relayNow},
			{Repo: openRepo, PR: 2, Lane: openLane, By: four, Child: open, Phase: state.Running, Started: long},
			{Repo: freshRepo, PR: 3, Lane: "fresh", By: four, Child: fresh, Phase: state.Running, Started: long},
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w.hungMerges(context.Background(), w.now)
	if l := out.String(); strings.Count(l, "MERGE HUNG") != 1 || !strings.Contains(l, `MERGE HUNG: o/hung#1 in lane scratch by "Agent four": merged at`) {
		t.Fatalf("watch lines:\n%s", l)
	}
	if !eventually(5*time.Second, func() bool { return !proc.Alive(hung) }) {
		t.Fatal("the hung devctl runs on")
	}
	if !proc.Alive(open) || !proc.Alive(fresh) {
		t.Error("a run that has not outlived its pull request was ended")
	}
	w.lostMerges(context.Background())
	if l := out.String(); !strings.Contains(l, "MERGE RECORDED: o/hung#1 exit 137, release unconfirmed (merged per GitHub)") {
		t.Fatalf("watch lines:\n%s", l)
	}
	st, err := w.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range st.Merges {
		want := map[string]string{hungRepo: state.Settling, nextRepo: state.Waiting, openRepo: state.Running, freshRepo: state.Running}[m.Repo]
		if m.Phase != want {
			t.Errorf("%s is %s, want %s", m.Key(), m.Phase, want)
		}
	}
	if evs, err := w.store.Events(10, func(e state.Event) bool { return e.Verb == "merge.hung" }); err != nil || len(evs) != 1 {
		t.Errorf("merge.hung events: %d, %v", len(evs), err)
	}
}

// lanes drop ends a running merge whose pull request merged while its
// devctl runs on, and records it; one whose pull request is open is refused.
func TestLanesDropEndsAHungRun(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	a := w.app
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
