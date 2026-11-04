package cmd

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	laneWaitRepo = "o/r"
	laneWaitNext = "next"
)

func TestLaneWaitingReasons(t *testing.T) {
	needsPlatform(t)
	now := time.Now()
	me := state.Party{Name: workerRole, Session: "s1"}
	next := state.Merge{Repo: laneWaitRepo, PR: 7, Lane: laneWaitRepo, By: me, Phase: state.Waiting, PID: os.Getpid(), Joined: now}
	a := queueApp(t)
	a.now = now
	a.cfg.Merge.SessionCap = 2
	a.cfg.Merge.Cap = 10
	running := func(repo string) state.Merge {
		return state.Merge{Repo: repo, PR: 1, Lane: repo, By: me, Phase: state.Running, PID: os.Getpid()}
	}
	st := &state.State{Merges: []state.Merge{running("o/a"), running("o/b"), next}}
	views := a.laneViews(st)
	out := a.capture(func() { a.printLanes(views) })
	for _, want := range []string{laneWaitNext, "merge.sessionCap 2", "o/a#1, o/b#1", `"worker" runs 2 merges`} {
		if !strings.Contains(out, want) {
			t.Errorf("lanes missing %q: %s", want, out)
		}
	}
	st.Agents = []state.Agent{{Party: me}}
	actual := a.agentViews(st, nil)
	if len(actual) != 1 || len(actual[0].MergeWaiting) != 1 || !strings.Contains(actual[0].MergeWaiting[0], "merge.sessionCap 2") {
		t.Fatalf("agent waiting reasons: %+v", actual)
	}
	// Both the full roster and its delta must retain the reason untruncated.
	av := agentView{agentFields: agentFields(state.Agent{Party: me}), MergeWaiting: []string{a.mergeWaitReason(st, next)}}
	out = a.capture(func() { a.printAgents([]agentView{av}) })
	if !strings.Contains(out, "merge.sessionCap 2") || !strings.Contains(a.agentFacts([]agentView{av})[0].Line, "o/a#1, o/b#1") {
		t.Errorf("agents lost reason: %s", out)
	}
	st.Merges = []state.Merge{next}
	if why := a.mergeWaitReason(st, next); !strings.Contains(why, "no local blocker") {
		t.Errorf("free: %s", why)
	}
	st.Holds = []state.Hold{{Target: laneWaitRepo, Reason: "release check", By: me, Until: now.Add(time.Hour)}}
	if why := a.mergeWaitReason(st, next); !strings.Contains(why, "gate hold") || !strings.Contains(why, "release check") {
		t.Errorf("held: %s", why)
	}
	st.Holds = nil
	st.Merges = []state.Merge{running("o/a"), next}
	a.cfg.Merge.Cap = 1
	if why := a.mergeWaitReason(st, next); !strings.Contains(why, `slots held by o/a#1 by "worker"`) {
		t.Errorf("cap: %s", why)
	}
	next.PID = 0
	if why := a.mergeWaitReason(st, next); !strings.Contains(why, "gate not running") {
		t.Errorf("absent: %s", why)
	}
}
