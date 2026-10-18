//go:build unix

package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// cutWatch is a watch whose state holds merge m of the registered agent
// four, its wakes recorded.
func cutWatch(t *testing.T, m state.Merge) (*watcher, *[]string) {
	t.Helper()
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: four}}
		st.Merges = []state.Merge{m}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	var woke []string
	was := wakeOwner
	wakeOwner = func(_ *app, _ context.Context, _ state.Party, q, msg, _ string) error {
		woke = append(woke, q+": "+msg)
		return nil
	}
	t.Cleanup(func() { wakeOwner = was })
	return w, &woke
}

// polls runs the watch's merge and owed-outcome steps n times.
func polls(w *watcher, n int) {
	for range n {
		w.lostMerges(context.Background())
		w.owedOutcomes(context.Background())
	}
}

// cutQueuedRun leaves the files of a queued run's merge-child cut with the
// machine: its pid, dead, and no exit code. It returns their base.
func cutQueuedRun(t *testing.T, w *watcher) string {
	t.Helper()
	base := filepath.Join(w.store.Dir(), ownedRuns, "pr-merge-o_r-7-4242")
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".pid", []byte(strconv.Itoa(deadPID(t))), 0o600); err != nil {
		t.Fatal(err)
	}
	return base
}

// A merge whose gate and devctl were killed with the machine before devctl
// wrote its exit code is recorded as cut, and the watch wakes its owner
// with what GitHub says, once.
func TestTheWatchWakesTheOwnerOfACutMerge(t *testing.T) {
	stubGitHub(t, github.Merged, "")
	stubPull(t, github.Pull{State: github.Merged, MergedAt: mergedAt}, nil)
	w, woke := cutWatch(t, state.Merge{Repo: scratchRepo, PR: 7, Lane: hungLane, By: four, PID: deadPID(t), Child: deadPID(t),
		Phase: state.Running, Started: relayNow})
	polls(w, 3)
	if len(*woke) != 1 || !strings.HasPrefix((*woke)[0], "s4: devctl pr merge o/r 7 was stopped by a shutdown or a crash") ||
		!strings.Contains((*woke)[0], "GitHub says o/r#7 merged at 2026-10-14T19:47:02Z") || !strings.Contains((*woke)[0], "`devctl release wait o/r --pr 7`") {
		t.Fatalf("woke %q", *woke)
	}
	evs, _ := w.store.Events(0, func(e state.Event) bool { return e.Verb == "merge.cut" })
	if len(evs) != 1 || !strings.HasSuffix(evs[0].Detail, `its outcome is owed to "Agent four"`) {
		t.Errorf("merge.cut events %+v", evs)
	}
	if st, _ := w.store.Read(); len(st.Owed) != 0 || len(st.Merges) != 1 || st.Merges[0].Phase != state.Settling {
		t.Errorf("owed %+v, merges %+v", st.Owed, st.Merges)
	}
}

// A queued merge's own run cut while it waited for its turn wakes its owner
// with the pull request still open, once; the place stays for a rerun.
func TestTheWatchWakesTheOwnerOfACutQueuedRun(t *testing.T) {
	stubGitHub(t, github.Open, "")
	w, woke := cutWatch(t, state.Merge{Repo: scratchRepo, PR: 7, Lane: hungLane, By: four, PID: deadPID(t), Phase: state.Waiting,
		Joined: relayNow, Seen: relayNow})
	base := cutQueuedRun(t, w)
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges[0].Run = base
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	polls(w, 3)
	if len(*woke) != 1 || !strings.HasSuffix((*woke)[0], "GitHub says o/r#7 is open, not merged: run the same command again") {
		t.Fatalf("woke %q", *woke)
	}
	if st, _ := w.store.Read(); len(st.Merges) != 1 || st.Merges[0].Phase != state.Waiting || st.Merges[0].Run != "" {
		t.Errorf("merges %+v", st.Merges)
	}
	if _, err := os.Stat(base + ".pid"); err == nil {
		t.Error("the cut run's files are left")
	}
}

// A queued run whose merge-child ended owed its outcome itself and removed
// its files: nobody is woken a second time.
func TestAFinishedQueuedRunIsNoCut(t *testing.T) {
	stubGitHub(t, github.Open, "")
	run := filepath.Join(t.TempDir(), ownedRuns, "pr-merge-o_r-7-4242")
	w, woke := cutWatch(t, state.Merge{Repo: scratchRepo, PR: 7, Lane: hungLane, By: four, PID: deadPID(t), Phase: state.Waiting,
		Joined: relayNow, Seen: relayNow, Seeded: true, Run: run})
	polls(w, 2)
	if st, _ := w.store.Read(); len(*woke) != 0 || len(st.Owed) != 0 {
		t.Errorf("woke %q, owed %+v", *woke, st.Owed)
	}
}

// A merge-child whose devctl a signal ended without its document (exit 143)
// hands over what GitHub says, as for a stopped merge.
func TestADevctlEndedBySignalSaysWhatGitHubSays(t *testing.T) {
	a, r, woke := stoppedMerge(t)
	r.stop = 0
	stubPull(t, github.Pull{State: github.Merged, MergedAt: mergedAt}, nil)
	a.tellOwner(context.Background(), r)
	if len(*woke) != 1 || (*woke)[0] != "beekeeper gate → s1: "+stoppedMerged {
		t.Errorf("woke %q, want %q", *woke, stoppedMerged)
	}
}
