//go:build unix

package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A session that runs merge.sessionCap merges already has its next one, its
// lane free, wait in the lane without reading GitHub: no pull request read,
// no budget probe, exit 76 with the reason; beekeeper budget shows the
// session's two polling merges and the queued one.
func TestASessionsThirdMergeWaitsWithoutReadingGitHub(t *testing.T) {
	asked := stubGitHub(t, github.Open, "")
	_, launched := stubSelf(t)
	me := state.Party{Session: "s1", Name: ownerName}
	a := queueApp(t,
		state.Merge{Repo: "o/a", PR: 1, Lane: "o/a", By: me, PID: sleeper(t), Phase: state.Running, Joined: time.Now().Add(-time.Hour), Started: time.Now()},
		state.Merge{Repo: "o/b", PR: 1, Lane: "o/b", By: me, PID: sleeper(t), Phase: state.Running, Joined: time.Now().Add(-time.Hour), Started: time.Now()})
	a.cfg.Merge.SessionCap = 2

	var err error
	stderr := gateStderr(t, func() { err = a.gate(context.Background(), mergeArgv(scratchRepo), 0, 0, false) })
	if Code(err) != ExitGateQueued {
		t.Fatalf("exit %d (%v), want %d; stderr %s", Code(err), err, ExitGateQueued, stderr)
	}
	launched()
	if !strings.Contains(stderr, `"worker" runs 2 merges (o/a#1, o/b#1), merge.sessionCap 2: it waits without reading GitHub until one ends`) {
		t.Errorf("stderr %q", stderr)
	}
	if *asked != 0 {
		t.Errorf("the waiting merge read %d pull requests", *asked)
	}
	st, _ := a.store.Read()
	if st.Budget != nil {
		t.Errorf("the waiting merge probed the budget: %+v", st.Budget)
	}
	gated := gatedMerges(st, func(pid int) bool { return pid != 0 })
	if len(gated) != 1 {
		t.Fatalf("gated %+v", gated)
	}
	if line := gated[0].line(2); line != `gated merges of "worker": 2 polling GitHub (o/a#1, o/b#1), 1 queued without reading it (o/r#7), merge.sessionCap 2` {
		t.Errorf("budget line %q", line)
	}
}
