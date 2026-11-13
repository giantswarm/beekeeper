//go:build unix

package cmd

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
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
	g.a.cfg.Merge.BusyBudget = config.Duration{Duration: budget}
	return g
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
