package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

const midtaskFinding = "giantswarm/beekeeper: fixture probe failed; the command returned exit 1"

func runProblemTestCommand(t *testing.T, a *app, args ...string) {
	t.Helper()
	c := a.agentsCmd()
	c.SetArgs(args)
	c.SetOut(a.out)
	c.SetErr(a.out)
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentProblemCanonicalFindings(t *testing.T) {
	for name, whitespace := range map[string]string{"newline": "\n", "tab": "\t", "spaces": "   ", "mixed": " \r\n\t "} {
		t.Run(name, func(t *testing.T) {
			a, _ := noteApp(t)
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents = []state.Agent{{Party: state.Party{Name: agentOne}, Task: sshTestTask, AssignedAt: a.now}}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			finding := strings.ReplaceAll(midtaskFinding, " ", whitespace)
			runProblemTestCommand(t, a, "problem", finding)
			runProblemTestCommand(t, a, verbIdle, flagReport, "fixture finished", flagProblem, finding)
			events, err := a.store.Events(0, func(ev state.Event) bool { return ev.Verb == agentProblemEvent })
			if err != nil || len(events) != 1 || events[0].Detail != agentOne+": "+midtaskFinding {
				t.Fatalf("canonical log: %+v, %v", events, err)
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			var queued []string
			for _, report := range st.WorkerReports {
				queued = append(queued, report.Problems...)
			}
			if len(queued) != 1 || queued[0] != midtaskFinding {
				t.Fatalf("canonical pending findings: %q", queued)
			}
		})
	}
}

func TestAgentProblemReregisterAssignment(t *testing.T) {
	for _, idle := range []bool{false, true} {
		t.Run(fmt.Sprintf("idle=%t", idle), func(t *testing.T) {
			a, _ := noteApp(t)
			assignedAt := a.now
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents = []state.Agent{{Party: state.Party{Name: agentOne}, Task: sshTestTask, AssignedAt: assignedAt}}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			runProblemTestCommand(t, a, "problem", midtaskFinding)
			if idle {
				runProblemTestCommand(t, a, verbIdle, flagProblem, midtaskFinding)
			}
			a.now = a.now.Add(time.Second)
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				firePending(st, nil, a.now)
				_, err := registerAgent(st, st.Agents[0].Party, func(state.Party) bool { return true }, a.now)
				return nil, err
			}); err != nil {
				t.Fatal(err)
			}
			runProblemTestCommand(t, a, verbIdle, flagProblem, midtaskFinding)
			st, err := a.store.Read()
			if err != nil || !st.Agents[0].AssignedAt.Equal(assignedAt) || len(st.WorkerReports) != 0 {
				t.Fatalf("assignment identity or duplicate delivery: %+v, %v", st, err)
			}
			events, err := a.store.Events(0, func(ev state.Event) bool { return ev.Verb == agentProblemEvent })
			if err != nil || len(events) != 1 {
				t.Fatalf("duplicate after registration: %+v, %v", events, err)
			}
			runProblemTestCommand(t, a, "assign", agentOne, sshTestTask)
			runProblemTestCommand(t, a, "problem", midtaskFinding)
			events, err = a.store.Events(0, func(ev state.Event) bool { return ev.Verb == agentProblemEvent })
			if err != nil || len(events) != 2 {
				t.Fatalf("new assignment finding: %+v, %v", events, err)
			}
		})
	}
}

func TestAgentProblemLogsAndDeliversOnce(t *testing.T) {
	a, out := noteApp(t)
	run := func(args ...string) error {
		t.Helper()
		c := a.agentsCmd()
		c.SetArgs(args)
		c.SetOut(out)
		c.SetErr(out)
		c.SilenceUsage, c.SilenceErrors = true, true
		return c.Execute()
	}
	if err := run("problem", midtaskFinding); err == nil {
		t.Fatal("unregistered finding accepted")
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Name: agentOne, Session: "problem-test-session"}, Task: sshTestTask, AssignedAt: a.now, Park: &state.Park{On: "#1", Waits: "fixture wait"}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, finding := range []string{"", " ", noProblems, "blocked by #42"} {
		if err := run("problem", finding); err == nil {
			t.Fatalf("invalid finding %q accepted", finding)
		}
	}
	if err := run("problem", "  "+midtaskFinding+"  "); err != nil {
		t.Fatal(err)
	}
	events, err := a.store.Events(0, func(ev state.Event) bool { return ev.Verb == agentProblemEvent })
	if err != nil || len(events) != 1 || !strings.Contains(events[0].Detail, midtaskFinding) {
		t.Fatalf("immediate log: %+v, %v", events, err)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		if st.Agents[0].Task != sshTestTask || st.Agents[0].Park == nil || st.Agents[0].Done {
			t.Fatal("finding ended the worker's task or park")
		}
		if !st.Agents[0].ActivityAt.Equal(a.now.UTC()) {
			t.Fatal("finding did not refresh explicit roster activity")
		}
		lines, events := firePending(st, nil, a.now)
		if len(lines) != 1 || !strings.Contains(lines[0], "PROBLEM FOUND") {
			t.Fatalf("delivery: %q", lines)
		}
		return events, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Each CLI command reloads persisted receipts, even after watch cleared its queue.
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		_, err := registerAgent(st, st.Agents[0].Party, func(state.Party) bool { return true }, a.now)
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
	if err := run("problem", midtaskFinding); err != nil {
		t.Fatal(err)
	}
	if err := run(verbIdle, flagDone, flagReport, "completed the fixture", flagProblem, midtaskFinding, flagProblem, midtaskFinding); err != nil {
		t.Fatal(err)
	}
	events, err = a.store.Events(0, func(ev state.Event) bool { return ev.Verb == agentProblemEvent })
	if err != nil || len(events) != 1 {
		t.Fatalf("duplicate log: %+v, %v", events, err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	lines, _ := firePending(st, nil, a.now)
	if len(lines) != 1 || !strings.Contains(lines[0], "WORKER REPORT") {
		t.Fatalf("final delivery: %q", lines)
	}
	if err := run("problem", midtaskFinding); err == nil {
		t.Fatal("idle worker finding accepted")
	}
	a.now = a.now.Add(time.Second)
	if err := run("assign", agentOne, sshTestTask); err != nil {
		t.Fatal(err)
	}
	if err := run("problem", midtaskFinding); err != nil {
		t.Fatal(err)
	}
	events, err = a.store.Events(0, func(ev state.Event) bool { return ev.Verb == agentProblemEvent })
	if err != nil || len(events) != 2 {
		t.Fatalf("new assignment log: %+v, %v", events, err)
	}
}

func TestAgentProblemRemoteCoordinator(t *testing.T) {
	a, request := executionTestApp(t)
	control := execution.ControlRequest{Ref: request.Ref, ID: "problem-one", Sequence: 1, Operation: "problem", Data: mustExecutionJSON(t, strings.ReplaceAll(midtaskFinding, " ", "\n\t"))}
	var result execution.ControlResult
	if err := coordinatorTestCall(t, a, request, "control", control, &result, true); err == nil {
		t.Fatal("lost reply reported success")
	}
	if err := coordinatorTestCall(t, a, request, "control", control, &result, false); err != nil || result.Error != "" {
		t.Fatal(err, result.Error)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		lines, events := firePending(st, nil, a.now)
		if len(lines) == 0 {
			t.Fatal("remote finding was not delivered")
		}
		return events, nil
	}); err != nil {
		t.Fatal(err)
	}
	control.ID, control.Sequence, control.Operation = "report-after-problem", 2, execution.ControlReport
	control.Data = mustExecutionJSON(t, executionReport{Finished: true, Text: "fixture completed", Problems: []string{midtaskFinding}})
	if err := coordinatorTestCall(t, a, request, "control", control, &result, false); err != nil || result.Error != "" {
		t.Fatal(err, result.Error)
	}
	events, err := a.store.Events(0, func(ev state.Event) bool { return ev.Verb == agentProblemEvent })
	if err != nil || len(events) != 1 || events[0].By.Session != request.ConversationID {
		t.Fatalf("remote logs: %+v, %v", events, err)
	}
	// The coordinator validates raw controls too, before mutating receipts.
	st := &state.State{Agents: []state.Agent{{Party: state.Party{Session: "other-worker-952"}, Task: sshTestTask}}}
	invalid, _ := json.Marshal("blocked by #42")
	if _, _, err := a.applyExecutionControl(context.Background(), st, st.Agents[0].Party, execution.ControlRequest{Operation: "problem", Data: invalid}); err == nil || st.Agents[0].ReportedProblems != nil {
		t.Fatal("invalid remote finding changed receipts")
	}
	if events, err := a.recordWorkerProblem(st, &st.Agents[0], st.Agents[0].Party, midtaskFinding); err != nil || len(events) != 1 {
		t.Fatal("another worker's same finding was suppressed", err)
	}
}
