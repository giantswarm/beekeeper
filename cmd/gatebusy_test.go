//go:build unix

package cmd

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
)

// busyDoc is devctl's document of a head whose base moved during every one
// of its --update-branch waits.
const busyDoc = `{"exitCode":3,"verdict":"not_applicable","reason":"the pull request is behind main, which requires branches to be up to date (main moved during the wait) after 3 waits; run the merge again once main is quiet"}`

// busyDevctl puts a devctl on PATH that answers busyDoc on its first busy
// runs and merges after; it returns the file counting its runs.
func busyDevctl(t *testing.T, busy int) string {
	t.Helper()
	count := filepath.Join(t.TempDir(), "count")
	fakeDevctl(t, `n=$(cat `+count+` 2>/dev/null || echo 0); n=$((n+1)); echo $n >`+count+`
if [ $n -le `+strconv.Itoa(busy)+` ]; then echo "update-branch: merge of the base requested" >&2; echo '`+busyDoc+`'; exit 3; fi
echo '`+mergedDoc+`'`)
	return count
}

// busyGate is a running merge of o/r#7 with --update-branch under budget,
// devctl's next run after a busy base pause later.
func busyGate(t *testing.T, budget, pause time.Duration) *gateRun {
	t.Helper()
	noSystemd(t)
	was := busyPauses
	busyPauses = []time.Duration{pause}
	t.Cleanup(func() { busyPauses = was })
	stubGitHub(t, github.Open, "")
	g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}})
	g.argv = mergeArgv(scratchRepo, "--update-branch")
	g.cfg.Merge.BusyBudget = config.Duration{Duration: budget}
	return g
}

// A merge whose base moved during every one of devctl's update waits keeps
// its lane place, so no other gated merge of the repository moves its base,
// and devctl runs again after a pause until it lands: the caller reads one
// document, the merge's, and the budget spent is reported.
func TestABusyBaseRunsDevctlAgainInItsLanePlace(t *testing.T) {
	count := busyDevctl(t, 2)
	g := busyGate(t, time.Hour, 400*time.Millisecond)
	stdout := gateStdout(t)
	done, output := make(chan error, 1), make(chan string, 1)
	go func() {
		var err error
		output <- gateStderr(t, func() { err = g.runMerge() })
		done <- err
	}()
	for runs(t, count) < 1 {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // in the pause after the first run
	if q := merge.Queue(gateState(t, g), scratchRepo); q.Running == nil || q.Running.PR != 7 {
		t.Errorf("during the pause lane %s runs %+v, want o/r#7 holding its place", scratchRepo, q.Running)
	}
	stderr := <-output
	if err := <-done; err != nil {
		t.Fatalf("the merge after two busy runs: %v", err)
	}
	if n := runs(t, count); n != 3 {
		t.Errorf("devctl ran %d times, want 3", n)
	}
	if got := stdout(); got != mergedDoc {
		t.Errorf("the caller read %q, want the merge's document alone", got)
	}
	for _, want := range []string{
		"the base moved during every wait of devctl run 1 (", "of the busy-base budget 1h0m0s spent): the merge keeps its lane place, and devctl runs again in 400ms",
		"devctl run 3 landed the merge on the busy base",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if d := lastEvent(t, g, "merged"); !strings.Contains(d, "o/r#7 exit 0, release v1.2.4") {
		t.Errorf("merged event: %q", d)
	}
}

// A base still busy once merge.busyBudget is spent ends the run with
// devctl's exit 3, its reason naming the spent budget, and the merge leaves
// its lane.
func TestABusyBaseEndsOnceItsBudgetIsSpent(t *testing.T) {
	count := busyDevctl(t, 99)
	g := busyGate(t, 300*time.Millisecond, 100*time.Millisecond)
	stdout := gateStdout(t)
	var err error
	stderr := gateStderr(t, func() { err = g.runMerge() })
	if Code(err) != 3 {
		t.Fatalf("exit %d, want devctl's 3", Code(err))
	}
	if n := runs(t, count); n < 2 || n > 4 {
		t.Errorf("devctl ran %d times, want the runs the budget fits", n)
	}
	if got := stdout(); got != busyDoc {
		t.Errorf("the caller read %q, want the last document alone", got)
	}
	if !strings.Contains(stderr, busySpent+": ") || !strings.Contains(stderr, "merge.busyBudget 300ms; nothing merged") {
		t.Errorf("stderr names no spent budget:\n%s", stderr)
	}
	if d := lastEvent(t, g, "merge.failed"); !strings.Contains(d, busySpent) {
		t.Errorf("merge.failed event names no spent budget: %q", d)
	}
	if st := gateState(t, g); len(st.Merges) != 0 {
		t.Errorf("a run with nothing merged keeps a place: %+v", st.Merges)
	}
}

// Without --update-branch devctl's exit 3 is the caller's: devctl runs once.
func TestABehindMergeWithoutUpdateBranchRunsOnce(t *testing.T) {
	count := busyDevctl(t, 99)
	g := busyGate(t, time.Hour, 0)
	g.argv = mergeArgv(scratchRepo)
	gateStdout(t)
	if err := g.runMerge(); Code(err) != 3 {
		t.Fatalf("exit %d, want devctl's 3", Code(err))
	}
	if n := runs(t, count); n != 1 {
		t.Errorf("devctl ran %d times, want 1", n)
	}
}
