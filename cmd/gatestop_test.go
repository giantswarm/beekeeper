//go:build unix

package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// badGateway is devctl's document of a merge call GitHub answered with 502.
const badGateway = `{"exitCode":7,"verdict":"usage","reason":"PUT https://api.github.com/repos/o/r/pulls/7/merge: 502 Bad Gateway []"}`

// countingDevctl puts a devctl on PATH that answers badGateway on its first
// fails runs and merges after; it returns the file counting its runs.
func countingDevctl(t *testing.T, fails int) string {
	t.Helper()
	count := filepath.Join(t.TempDir(), "count")
	fakeDevctl(t, `n=$(cat `+count+` 2>/dev/null || echo 0); n=$((n+1)); echo $n >`+count+`
if [ $n -le `+strconv.Itoa(fails)+` ]; then echo "PUT .../merge: 502 Bad Gateway" >&2; echo '`+badGateway+`'; exit 7; fi
echo '`+mergedDoc+`'`)
	return count
}

// quickRetries sends a merge again at once.
func quickRetries(t *testing.T) {
	t.Helper()
	was := serverRetries
	serverRetries = []time.Duration{0, 0, 0}
	t.Cleanup(func() { serverRetries = was })
}

// gateStdout catches the gate's stdout in a file and returns its reader.
func gateStdout(t *testing.T) func() string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdout")
	f, err := os.Create(path) //nolint:gosec // the test's file
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = f
	t.Cleanup(func() { os.Stdout = stdout; _ = f.Close() })
	return func() string {
		raw, _ := os.ReadFile(path) //nolint:gosec // as above
		return strings.TrimSpace(string(raw))
	}
}

func runs(t *testing.T, count string) int {
	t.Helper()
	raw, _ := os.ReadFile(count) //nolint:gosec // the test's file
	n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return n
}

// A merge call GitHub answers with a 5xx is sent again while the pull
// request is open, and merges on the next success; the caller reads one
// document, the merge's.
func TestAMergeCallsServerErrorIsRetried(t *testing.T) {
	noSystemd(t)
	quickRetries(t)
	count := countingDevctl(t, 2)
	asked := stubGitHub(t, github.Open, "")
	stdout := gateStdout(t)
	g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}})
	if err := g.runMerge(); err != nil {
		t.Fatalf("the merge after two 502s: %v", err)
	}
	if n := runs(t, count); n != 3 {
		t.Errorf("devctl ran %d times, want 3", n)
	}
	if *asked != 2 {
		t.Errorf("GitHub asked %d times before the retries, want 2", *asked)
	}
	if got := stdout(); got != mergedDoc {
		t.Errorf("the caller read %q, want the merge's document alone", got)
	}
	if d := lastEvent(t, g, "merge.retry"); !strings.Contains(d, "o/r#7: a 5xx on the merge call, retry 2 of 3") {
		t.Errorf("merge.retry event: %q", d)
	}
	if d := lastEvent(t, g, "merged"); !strings.Contains(d, "o/r#7 exit 0, release v1.2.4") {
		t.Errorf("merged event: %q", d)
	}
	if st := gateState(t, g); len(st.Merges) != 0 {
		t.Errorf("merges left: %+v", st.Merges)
	}
}

// The retries spent, the last 502 is the run's outcome: nothing merged, and
// the merge leaves its lane with its run.
func TestAMergeCallsServerErrorEndsAfterTheRetries(t *testing.T) {
	noSystemd(t)
	quickRetries(t)
	count := countingDevctl(t, 99)
	stubGitHub(t, github.Open, "")
	stdout := gateStdout(t)
	g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}})
	if err := g.runMerge(); Code(err) != 7 {
		t.Fatalf("exit %d, want 7", Code(err))
	}
	if n := runs(t, count); n != 1+len(serverRetries) {
		t.Errorf("devctl ran %d times, want %d", n, 1+len(serverRetries))
	}
	if got := stdout(); got != badGateway {
		t.Errorf("the caller read %q, want the last document alone", got)
	}
	if d := lastEvent(t, g, "merge.failed"); !strings.Contains(d, "o/r#7 exit 7, nothing merged, it left lane "+scratchRepo) {
		t.Errorf("merge.failed event: %q", d)
	}
	if st := gateState(t, g); len(st.Merges) != 0 {
		t.Errorf("a run with nothing merged keeps a place: %+v", st.Merges)
	}
}

// A 502 whose merge landed after all is not sent again: GitHub judges it.
func TestAServerErrorOfALandedMergeIsNotRetried(t *testing.T) {
	noSystemd(t)
	quickRetries(t)
	count := countingDevctl(t, 99)
	stubGitHub(t, github.Merged, "")
	gateStdout(t)
	g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}})
	if err := g.runMerge(); Code(err) != 7 {
		t.Fatalf("exit %d, want devctl's 7", Code(err))
	}
	if n := runs(t, count); n != 1 {
		t.Errorf("devctl ran %d times, want 1", n)
	}
	if d := lastEvent(t, g, "merged"); !strings.Contains(d, "o/r#7 exit 7, release unconfirmed (merged per GitHub)") {
		t.Errorf("merged event: %q", d)
	}
}

// writeRC leaves the exit code a merge's devctl wrote for repo#pr.
func writeRC(t *testing.T, dir, repo string, pr, rc int) {
	t.Helper()
	base := mergeBase(dir, repo, pr)
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".rc", []byte(strconv.Itoa(rc)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A gate killed while its merge ran leaves no place after one watch tick:
// its devctl gone, or ended while its merge-child still hands the outcome
// over, the run is recorded, nothing merged, and leaves the lane.
func TestAKilledGateLeavesNoPlaceAfterOneTick(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	w.now = relayNow
	dir := w.store.Dir()
	stubPulls(t, map[int]github.Pull{1: {State: github.Open}, 2: {State: github.Open}})
	writeRC(t, dir, hungRepo, 1, 143)
	lingering := hungChild(t, dir, openRepo, 2)
	writeRC(t, dir, openRepo, 2, 143)
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{
			{Repo: hungRepo, PR: 1, Lane: hungLane, By: four, Phase: state.Running, Started: relayNow},
			{Repo: openRepo, PR: 2, Lane: openLane, By: four, Child: lingering, Phase: state.Running, Started: relayNow},
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w.lostMerges(context.Background())
	l := out.String()
	for _, want := range []string{"MERGE RECORDED: o/hung#1 exit 143, nothing merged, it left lane scratch", "MERGE RECORDED: o/open#2 exit 143, nothing merged, it left lane open"} {
		if !strings.Contains(l, want) {
			t.Errorf("no %q in the watch lines:\n%s", want, l)
		}
	}
	if st, err := w.store.Read(); err != nil || len(st.Merges) != 0 {
		t.Errorf("places left after one tick: %+v (%v)", st.Merges, err)
	}
}

// A TaskStop of the background task a gate runs ends the gate's merge: a
// running merge's devctl through its merge-child, a waiting merge's place
// (a seeded place stays). A task of another session, or one that runs no
// gate, is left alone.
func TestTaskStopEndsTheGatesMerge(t *testing.T) {
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	a := w.app
	dir := a.store.Dir()
	child := hungChild(t, dir, hungRepo, 1)
	tasks := filepath.Join(t.TempDir(), "tasks")
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{
			{Repo: hungRepo, PR: 1, Lane: hungLane, By: four, PID: 1, Child: child, Output: filepath.Join(tasks, "brun.output"), Phase: state.Running},
			{Repo: nextRepo, PR: 4, Lane: hungLane, By: four, PID: 1, Output: filepath.Join(tasks, "bwait.output"), Phase: state.Waiting},
			{Repo: nextRepo, PR: 5, Lane: hungLane, By: four, PID: 1, Output: filepath.Join(tasks, "bwait.output"), Phase: state.Waiting, Seeded: true},
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if said := a.stopTask("another session", "brun"); said != "" {
		t.Errorf("another session's TaskStop: %q", said)
	}
	if said := a.stopTask(four.Session, "bnone"); said != "" {
		t.Errorf("a task without a gate: %q", said)
	}
	if !proc.Alive(child) {
		t.Fatal("a TaskStop that names no gate ended a devctl")
	}
	if said := a.stopTask(four.Session, "brun"); !strings.Contains(said, "o/hung#1's devctl (merge-child pid "+strconv.Itoa(child)+") is ended") {
		t.Errorf("the running merge's TaskStop: %q", said)
	}
	if !eventually(5*time.Second, func() bool { return !proc.Alive(child) }) {
		t.Error("the stopped gate's merge-child runs on")
	}
	if said := a.stopTask(four.Session, "bwait"); !strings.Contains(said, "o/next#4 leaves lane scratch") {
		t.Errorf("the waiting merge's TaskStop: %q", said)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Merges) != 2 || st.Merges[0].Phase != state.Running || st.Merges[1].PR != 5 || st.Merges[1].Output != "" {
		t.Errorf("want the running merge, for its gate to record, and the seeded place: %+v", st.Merges)
	}
	if evs, err := a.store.Events(10, func(e state.Event) bool { return e.Verb == "merge.stopped" }); err != nil || len(evs) != 3 {
		t.Errorf("merge.stopped events: %d, %v", len(evs), err)
	}
}

// Stopping a gated merge's background task leaves no process of it: the
// hook ends its devctl and merge-child, and the gate records the run, which
// merged nothing, its place gone.
func TestStoppingTheGateLeavesNoChild(t *testing.T) {
	noSystemd(t)
	devpid := filepath.Join(t.TempDir(), "devctl.pid")
	fakeDevctl(t, `echo $$ >`+devpid+`; exec sleep 60`)
	stubGitHub(t, github.Open, "")
	gateStdout(t)
	g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}})
	g.cfg.StateDir = g.store.Dir()
	output := filepath.Join(t.TempDir(), "tasks", "bgate.output")
	if err := g.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges[0].Output = output
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- g.runMerge() }()
	var dev int
	if !eventually(10*time.Second, func() bool {
		raw, err := os.ReadFile(devpid) //nolint:gosec // the test's file
		dev, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && dev > 0 && gateState(t, g).Merges[0].Child != 0
	}) {
		t.Fatal("devctl did not start")
	}
	child := gateState(t, g).Merges[0].Child
	if said := g.stopTask("", "bgate"); said == "" {
		t.Fatal("the TaskStop of the gate's task ended nothing")
	}
	select {
	case err := <-done:
		if Code(err) != 143 {
			t.Errorf("exit %d, want 143", Code(err))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the gate did not end with its devctl")
	}
	if !eventually(5*time.Second, func() bool { return !proc.Alive(dev) && !proc.Alive(child) }) {
		t.Errorf("left running: devctl %v, merge-child %v", proc.Alive(dev), proc.Alive(child))
	}
	if st := gateState(t, g); len(st.Merges) != 0 {
		t.Errorf("places left: %+v", st.Merges)
	}
}
