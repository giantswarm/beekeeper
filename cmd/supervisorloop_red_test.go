package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	loopLintURL  = "https://github.com/o/r/actions/runs/5/job/50"
	loopBuildURL = "https://example.com/ci/build/6"
)

// The rollup's red checks carry their names and URLs: a check run's name
// and details URL, a commit status's context and target URL. Pending,
// green and neutral checks are not red.
func TestParseLoopPullReadsRedChecks(t *testing.T) {
	rollup := `[{"status":"COMPLETED","conclusion":"FAILURE","name":"pre-commit","detailsUrl":"` + loopLintURL + `"},
	  {"status":"COMPLETED","conclusion":"CANCELLED","name":"integration","detailsUrl":"u-integration"},
	  {"status":"COMPLETED","conclusion":"TIMED_OUT","name":"slow","detailsUrl":"u-slow"},
	  {"state":"ERROR","context":"ci/build","targetUrl":"` + loopBuildURL + `"},
	  {"status":"IN_PROGRESS","conclusion":"","name":"running"},
	  {"status":"COMPLETED","conclusion":"SUCCESS","name":"unit"},
	  {"status":"COMPLETED","conclusion":"NEUTRAL","name":"neutral"},
	  {"state":"PENDING","context":"ci/pending"}]`
	p, err := parseLoopPull(loopPR, []byte(`{"state":"OPEN","headRefOid":"x","statusCheckRollup":`+rollup+`}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []superloop.Check{{Name: "ci/build", URL: loopBuildURL}, {Name: "integration", URL: "u-integration"}, {Name: "pre-commit", URL: loopLintURL}, {Name: "slow", URL: "u-slow"}}
	if p.Green || len(p.Red) != len(want) {
		t.Fatalf("green %v, red %+v", p.Green, p.Red)
	}
	for i := range want {
		if p.Red[i] != want[i] {
			t.Errorf("red[%d] = %+v, want %+v", i, p.Red[i], want[i])
		}
	}
	p, err = parseLoopPull(loopPR, []byte(`{"state":"OPEN","headRefOid":"x","statusCheckRollup":[{"status":"QUEUED","conclusion":""}]}`))
	if err != nil || len(p.Red) != 0 {
		t.Errorf("pending checks read as red: %+v, %v", p.Red, err)
	}
}

// remote makes the rig's worker a managed SSH execution.
func (r *loopRig) remote(t *testing.T) {
	t.Helper()
	ref := execution.Ref{Host: execution.HostID(sshTestSelector), Execution: execution.ExecutionID("11112222-3333-4444-5555-666677778888"), Generation: 1}
	err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		for i := range st.Agents {
			if st.Agents[i].Name == loopWorker {
				st.Agents[i].Execution = &ref
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A worker parked on its pull request while its checks run is woken once a
// check on the parked head goes red, local or on an execution host alike,
// with the check's name and URL; a later pass and a restarted loop wake it
// no second time, and no review starts.
func TestLoopWakesAWorkerOnARedCheck(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "on the coordinator", true: "on an execution host"}[remote], func(t *testing.T) {
			r := newLoopRig(t)
			r.park(t, loopPR, time.Now())
			if remote {
				r.remote(t)
			}
			r.pull.Green = false
			r.pass(t)
			if c := r.called(agentsName, agentWakeName); c != nil {
				t.Fatalf("pending checks woke %v", c)
			}
			r.pull.Red = []superloop.Check{{Name: "pre-commit", URL: loopLintURL}}
			r.pass(t)
			wake := r.called(agentsName, agentWakeName, loopWorker)
			if wake == nil || !strings.Contains(wake[3], "pre-commit "+loopLintURL) || !strings.Contains(wake[3], loopURL) {
				t.Fatalf("no red wake: %v\n%s", r.calls, r.out)
			}
			if c := r.called(agentsName, agentStartName); c != nil {
				t.Errorf("a red head started %v", c)
			}
			if evs := r.logged(t, "supervisor.red"); len(evs) != 1 || !strings.Contains(evs[0].Detail, "pre-commit") {
				t.Errorf("red logged as %+v", evs)
			}
			// The same failure again: nobody is woken twice.
			r.pass(t)
			if c := r.called(agentsName, agentWakeName); c != nil {
				t.Errorf("the same red check woke %v again", c)
			}
			// The fix pushed and parked on, green: the review starts as
			// before.
			r.pull.Head, r.pull.Green, r.pull.Red = loopFixed, true, nil
			err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				for i := range st.Agents {
					if st.Agents[i].Name == loopWorker {
						st.Agents[i].Park = &state.Park{On: loopPR, At: time.Now().Add(time.Minute), Waits: "desk review", ReviewHead: loopFixed}
					}
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			r.pass(t)
			if c := r.called(agentsName, agentStartName, loopReviewer); c == nil {
				t.Errorf("no review of the green fixed head: %v\n%s", r.calls, r.out)
			}
			if c := r.called(agentsName, agentWakeName); c != nil {
				t.Errorf("a green head woke %v", c)
			}
		})
	}
}
