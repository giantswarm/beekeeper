package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestExecutionCIParkSurvivesLostReplyAndWakesOnResult(t *testing.T) {
	for _, result := range []string{ciSucceeded, ciLintRed} {
		t.Run(result, func(t *testing.T) {
			a, r := executionTestApp(t)
			previousReviewGH := executionReviewGH
			t.Cleanup(func() { executionReviewGH = previousReviewGH })
			executionReviewGH = func(context.Context, ...string) ([]byte, error) {
				return []byte(fmt.Sprintf(`{"head":{"ref":%q,"sha":%q,"repo":{"full_name":%q}}}`, executionBranch(r.Ref), ciHead, r.Repository)), nil
			}

			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Starts[0].Execution.Request.Messaging = true
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			control := execution.ControlRequest{Ref: r.Ref, ID: "ci-park", Sequence: 1, Operation: execution.ControlPark,
				Data: mustExecutionJSON(t, executionPark{On: sshTestRepository + "#7", Waits: "CI before desk review", Checks: true})}
			var receipt execution.ControlResult
			if err := coordinatorTestCall(t, a, r, "control", control, &receipt, true); err == nil {
				t.Fatal("lost reply reported success")
			}
			if err := coordinatorTestCall(t, a, r, "control", control, &receipt, false); err != nil || receipt.Error != "" {
				t.Fatalf("retry: %+v %v", receipt, err)
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			p := st.Agents[0].Park
			if p == nil || !p.Checks || p.ReviewHead != "" || p.PostMerge != nil || st.Starts[0].Execution.Completed || len(st.ReviewParks) != 0 || len(st.Timers) != 1 {
				t.Fatalf("CI wait became review/completion or duplicated: park %+v, timers %+v", p, st.Timers)
			}
			w := &watcher{app: a}
			if settled := w.settledParks(context.Background(), st); len(settled) != 0 {
				t.Fatalf("ordinary PR settlement handled CI park: %+v", settled)
			}
			if !timerSettlesPark(st.Timers[0], p, a.cfg.Guide.Person) {
				t.Fatal("CI timer cannot wake its park")
			}
			answer := ciAnswer("OPEN", ciRunning)
			readErr := errors.New("CI read unavailable")
			fakeRead(t, func(string) ([]byte, error) { return answer, readErr })
			found := checkTimers(context.Background(), st.Timers, a.now, false)
			_, _, fires, _ := settleTimers(st, found, a.now)
			if len(fires) != 0 || len(st.Timers) != 1 {
				t.Fatal("unreadable CI woke worker")
			}
			readErr = nil
			found = checkTimers(context.Background(), st.Timers, a.now.Add(timerEvery), false)
			_, _, fires, _ = settleTimers(st, found, a.now.Add(timerEvery))
			if len(fires) != 0 || len(st.Timers) != 1 {
				t.Fatal("running CI woke worker")
			}
			answer = ciAnswer("OPEN", result)
			now := a.now.Add(2 * timerEvery)
			found = checkTimers(context.Background(), st.Timers, now, false)
			_, _, fires, _ = settleTimers(st, found, now)
			if len(fires) != 1 || len(st.Timers) != 1 || fires[0].t.Wake != r.Name || !strings.Contains(fires[0].message(), "CI ") {
				t.Fatalf("settled CI did not produce worker wake: %+v", fires)
			}
			// Persist settlement, then lose the in-memory fires as a restart would.
			if err := a.store.Update(func(saved *state.State) ([]state.Event, error) {
				saved.Timers = st.Timers
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			restarted, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			_, _, fires, _ = settleTimers(restarted, found, now)
			if len(fires) != 1 {
				t.Fatal("restart lost the CI wake before outbox insertion")
			}
			w = &watcher{app: a}
			// A replaced park cannot consume an old timer's wake.
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents[0].Park.At = p.At.Add(time.Second)
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			w.wakeTimer(context.Background(), fires[0])
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				if len(st.Starts[0].Execution.Messages) != 0 {
					t.Error("stale timer woke replacement park")
				}
				if len(st.Timers) != 0 {
					t.Error("obsolete CI timer was retained")
				}
				st.Agents[0].Park.At = p.At
				st.Timers = restarted.Timers
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			w.wakeTimer(context.Background(), fires[0])
			stored, err := a.store.Read()
			if err != nil || stored.Agents[0].Park == nil || len(stored.Starts[0].Execution.Messages) != 0 || len(stored.Timers) != 0 {
				t.Fatalf("CI used the old execution: %v", err)
			}
			park := stored.Agents[0].Park
			if result == ciSucceeded {
				if park.Checks || park.ReviewHead != ciHead || len(stored.ReviewParks) != 1 {
					t.Fatalf("green CI not handed to review: %+v", park)
				}
			} else if len(park.Continuation) != 1 {
				t.Fatalf("red CI lost its fresh continuation: %+v", park)
			}
			w.wakeTimer(context.Background(), fires[0])
			replayed, _ := a.store.Read()
			if len(replayed.Starts[0].Execution.Messages) != 0 || len(replayed.Agents[0].Park.Continuation) != len(park.Continuation) {
				t.Fatal("replay duplicated a delivery")
			}

		})
	}
}

// CI that never settles wakes the worker too, at the wait a merge gives it.
func TestExecutionCIParkWakesAtItsDeadline(t *testing.T) {
	a, r := executionTestApp(t)
	a.cfg.Merge.CITimeout = config.Duration{Duration: time.Hour}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Request.Messaging = true
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	control := execution.ControlRequest{Ref: r.Ref, ID: "ci-deadline", Sequence: 1, Operation: execution.ControlPark,
		Data: mustExecutionJSON(t, executionPark{On: sshTestRepository + "#7", Waits: "CI before desk review", Checks: true})}
	var receipt execution.ControlResult
	if err := coordinatorTestCall(t, a, r, "control", control, &receipt, false); err != nil || receipt.Error != "" {
		t.Fatalf("park: %+v %v", receipt, err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Timers) != 1 || st.Timers[0].Until.IsZero() {
		t.Fatalf("CI wait has no deadline: %+v", st.Timers)
	}
	_, _, fires, _ := settleTimers(st, map[int]checkResult{}, st.Timers[0].Until.Add(time.Minute))
	if len(fires) != 1 || fires[0].t.Wake != r.Name || !fires[0].timedOut {
		t.Fatalf("deadline did not wake the worker: %+v", fires)
	}
}

func TestExecutionCIParkRejectsInvalidTargets(t *testing.T) {
	for _, on := range []string{"", "#1", litHoldMerges, "other/repo#7"} {
		t.Run(on, func(t *testing.T) {
			a, _ := executionTestApp(t)
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = a.applyExecutionControl(context.Background(), st, st.Agents[0].Party, execution.ControlRequest{Operation: execution.ControlPark,
				Data: mustExecutionJSON(t, executionPark{On: on, Waits: "CI", Checks: true})})
			if err == nil || st.Agents[0].Park != nil || len(st.Timers) != 0 {
				t.Fatalf("invalid CI target changed state: %v", err)
			}
		})
	}
}

func TestExecutionCheckRejectsHostWithoutCIWait(t *testing.T) {
	a, r := executionTestApp(t)
	host := a.cfg.Agents.Hosts[sshTestSelector]
	a.executionProbe = func(context.Context, execution.Host) (execution.Capabilities, error) {
		return execution.Capabilities{SourcePreparation: true, Publication: true, Protocol: execution.Protocol, Identity: r.Ref.Host, PolicyDigest: host.PolicyDigest,
			Harnesses: map[string]string{execution.HarnessClaude: publicationClaudeVersion}, Resources: execution.Resources{ObservedAt: time.Now(), AvailableMiB: 1 << 16}}, nil
	}
	if _, err := a.probeExecution(context.Background(), host); err == nil || !strings.Contains(err.Error(), "CI wait") {
		t.Fatalf("host without CI wait accepted: %v", err)
	}
}

func TestExecutionCIParkRequiresWakeRoute(t *testing.T) {
	a, _ := executionTestApp(t)
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = a.applyExecutionControl(context.Background(), st, st.Agents[0].Party, execution.ControlRequest{Operation: execution.ControlPark,
		Data: mustExecutionJSON(t, executionPark{On: sshTestRepository + "#7", Waits: "CI", Checks: true})})
	if err == nil || !strings.Contains(err.Error(), "wake route") || st.Agents[0].Park != nil || len(st.Timers) != 0 {
		t.Fatalf("CI park without wake route accepted: %v", err)
	}
}
