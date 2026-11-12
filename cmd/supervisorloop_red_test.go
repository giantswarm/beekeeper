package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	loopLintURL         = "https://github.com/o/r/actions/runs/5/job/50"
	loopBuildURL        = "https://example.com/ci/build/6"
	loopWorkflowHead    = "head"
	loopOtherWorkflow   = "other-ci"
	loopWorkflowSuccess = "success"
	loopWorkflowFailure = "failure"
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
	p, err := ParseLoopPull(loopPR, []byte(`{"state":"OPEN","headRefOid":"x","statusCheckRollup":`+rollup+`}`))
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
	p, err = ParseLoopPull(loopPR, []byte(`{"state":"OPEN","headRefOid":"x","statusCheckRollup":[{"status":"QUEUED","conclusion":""}]}`))
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
						st.Agents[i].Park = &state.Park{On: loopPR, At: time.Now().Add(time.Minute), Waits: deskReviewWait, ReviewHead: loopFixed}
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

// Parsing feeds both the wake decision and the approved head's green gate.
func TestLoopSupersededChecks(t *testing.T) {
	old := LoopCheck{Name: "test-check", WorkflowName: "ci", Status: CheckCompleted, Conclusion: "CANCELLED", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), DetailsURL: "https://github.com/o/r/actions/runs/10/job/1"}
	newer := old
	newer.StartedAt = old.StartedAt.Add(time.Minute)
	newer.Conclusion = CheckSuccess
	newer.DetailsURL = "https://github.com/o/r/actions/runs/11/job/2"
	pending := newer
	pending.Status, pending.Conclusion, pending.StartedAt = "QUEUED", "", time.Time{}
	other := newer
	other.Name, other.WorkflowName = "lint", "lint"
	workflow := newer
	workflow.Name = "renamed unit"
	sameRun := workflow
	sameRun.DetailsURL = "https://github.com/o/r/actions/runs/10/job/3"
	older := newer
	older.StartedAt = old.StartedAt.Add(-time.Minute)
	older.DetailsURL = "https://github.com/o/r/actions/runs/9/job/2"
	external := newer
	external.DetailsURL = "https://ci.example/check/2"
	externalOld := old
	externalOld.DetailsURL = "https://ci.example/check/1"
	failed := old
	failed.Conclusion = CheckFailure
	timedOut := old
	timedOut.Conclusion = "TIMED_OUT"
	externalFailed := externalOld
	externalFailed.Conclusion = CheckFailure
	rerun := newer
	rerun.DetailsURL = old.DetailsURL
	otherWorkflow := newer
	otherWorkflow.WorkflowName = loopOtherWorkflow
	otherWorkflowPending := pending
	otherWorkflowPending.WorkflowName = loopOtherWorkflow
	otherWorkflowExternal := external
	otherWorkflowExternal.WorkflowName = loopOtherWorkflow
	delayedOld := failed
	delayedOld.StartedAt = newer.StartedAt.Add(time.Minute)
	for _, tt := range []struct {
		name       string
		checks     []LoopCheck
		green, red bool
	}{
		{"newer success", []LoopCheck{old, newer}, true, false},
		{"reverse order", []LoopCheck{newer, old}, true, false},
		{"older success", []LoopCheck{old, older}, false, true},
		{"external check timestamp", []LoopCheck{externalOld, external}, true, false},
		{"commit status is not replacement", []LoopCheck{old, {Name: "test-check", State: CheckSuccess}}, false, true},
		{"lone cancellation", []LoopCheck{old}, false, true},
		{"queued replacement", []LoopCheck{old, pending}, false, false},
		{"different check", []LoopCheck{old, other}, false, true},
		{"same workflow newer run", []LoopCheck{old, workflow}, true, false},
		{"same workflow same run", []LoopCheck{old, sameRun}, false, true},
		{"failed run superseded by success on same head", []LoopCheck{failed, newer}, true, false},
		{"failed run reverse order", []LoopCheck{newer, failed}, true, false},
		{"failed run superseded by queued run", []LoopCheck{failed, pending}, false, false},
		{"same name in different workflow stays red", []LoopCheck{failed, otherWorkflow}, false, true},
		{"same name in different workflow reverse order", []LoopCheck{otherWorkflow, failed}, false, true},
		{"different workflow queued job does not hide failure", []LoopCheck{failed, otherWorkflowPending}, false, true},
		{"different external workflow does not hide failure", []LoopCheck{externalFailed, otherWorkflowExternal}, false, true},
		{"different failed check stays red", []LoopCheck{failed, other}, false, true},
		{"failed job not replaced by renamed workflow job", []LoopCheck{failed, workflow}, false, true},
		{"external failure superseded by timestamp", []LoopCheck{externalFailed, external}, true, false},
		{"same workflow run rerun by timestamp", []LoopCheck{failed, rerun}, true, false},
		{"run ID orders delayed starts", []LoopCheck{delayedOld, newer}, true, false},
		{"timeout superseded by success", []LoopCheck{timedOut, newer}, true, false},
		{"newer failure wins", []LoopCheck{older, failed}, false, true},
		{"newer queued run supersedes green", []LoopCheck{older, pending}, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newLoopRig(t)
			r.park(t, loopPR, time.Now())
			r.pass(t)
			r.verdict("R1", loopHead, superloop.Approved, "")
			raw, err := json.Marshal(LoopPullJSON{State: superloop.Open, Head: loopHead, Rollup: tt.checks})
			if err != nil {
				t.Fatal(err)
			}
			p, err := ParseLoopPull(loopPR, raw)
			if err != nil {
				t.Fatal(err)
			}
			if p.Green != tt.green || (len(p.Red) > 0) != tt.red {
				t.Fatalf("green=%v red=%v", p.Green, p.Red)
			}
			r.pull.Green, r.pull.Red = p.Green, p.Red
			r.pass(t)
			if got := len(r.logged(t, "supervisor.red")) > 0; got != tt.red {
				t.Errorf("red wake=%v, want %v", got, tt.red)
			}
			if got := len(r.logged(t, "supervisor.merge")) > 0; got != tt.green {
				t.Errorf("merge=%v, want %v", got, tt.green)
			}
		})
	}
}

// A workflow may fail before it creates ci-ok. The old ci-ok success in
// the rollup must not let that head authorize a merge.
func TestLoopNewestWorkflowWithoutRequiredJob(t *testing.T) {
	old := LoopWorkflowRun{ID: 10, WorkflowID: 1, Attempt: 1, Head: loopWorkflowHead, Name: "CI", Status: workerGitHubCompleted, Conclusion: loopWorkflowSuccess}
	newRun := old
	newRun.ID = 20
	other := old
	other.WorkflowID = 2
	other.ID = 30
	other.Name = loopOtherWorkflow
	for _, tc := range []struct {
		name       string
		status     string
		conclusion string
		missing    bool
		green      bool
		red        int
	}{
		{"failure before job creation", workerGitHubCompleted, loopWorkflowFailure, true, false, 1},
		{"queued before job creation", epicChildQueued, "", true, false, 0},
		{"successful suite missing job", workerGitHubCompleted, loopWorkflowSuccess, true, false, 0},
		{"successful replacement", workerGitHubCompleted, loopWorkflowSuccess, false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newRun.Status, newRun.Conclusion = tc.status, tc.conclusion
			for _, runs := range [][]LoopWorkflowRun{{old, newRun, other}, {other, newRun, old}} {
				// Separate pages ensure pagination and response order do not
				// change which suite is authoritative.
				var raw []byte
				for _, run := range runs {
					page, err := json.Marshal(map[string]any{"workflow_runs": []LoopWorkflowRun{run}})
					if err != nil {
						t.Fatal(err)
					}
					raw = append(raw, page...)
				}
				id := "10"
				if !tc.missing {
					id = "20"
				}
				rollup := []LoopCheck{{Name: "ci-ok", WorkflowName: "CI", Status: "COMPLETED", Conclusion: "SUCCESS", DetailsURL: "https://github.com/o/r/actions/runs/" + id + "/job/1"}}
				p := superloop.Pull{Head: loopWorkflowHead, Green: true}
				if err := ApplyLoopWorkflowRuns(&p, raw, rollup); err != nil {
					t.Fatal(err)
				}
				if p.Green != tc.green || len(p.Red) != tc.red {
					t.Fatalf("green %v red %+v", p.Green, p.Red)
				}
			}
		})
	}
}

func TestLoopWorkflowRunsUnreadable(t *testing.T) {
	for _, raw := range []string{"", "{}", "{", `{"workflow_runs":[{"head_sha":"head"}]}`} {
		p := superloop.Pull{Head: loopWorkflowHead, Green: true}
		if err := ApplyLoopWorkflowRuns(&p, []byte(raw), nil); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestLoopWorkflowRunAttemptsAndOtherHeads(t *testing.T) {
	p := superloop.Pull{Head: loopWorkflowHead, Green: true}
	raw := `{"workflow_runs":[
		{"id":20,"workflow_id":1,"run_attempt":2,"head_sha":"head","status":"completed","conclusion":"success"},
		{"id":20,"workflow_id":1,"run_attempt":1,"head_sha":"head","status":"completed","conclusion":"failure"},
		{"id":30,"workflow_id":1,"run_attempt":1,"head_sha":"other","status":"completed","conclusion":"failure"}
	]}`
	if err := ApplyLoopWorkflowRuns(&p, []byte(raw), nil); err != nil {
		t.Fatal(err)
	}
	if !p.Green || len(p.Red) != 0 {
		t.Fatalf("green %v red %+v", p.Green, p.Red)
	}
}
