package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/watch"
	"github.com/giantswarm/beekeeper/pkg/project"
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
	for _, tc := range []struct {
		name     string
		idle     bool
		replaced bool
	}{
		{name: "own-open"},
		{name: "own-idle", idle: true},
		{name: "replacement-open", replaced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := noteApp(t)
			assignedAt := a.now
			original := state.Party{Name: agentOne, Session: "original-session"}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents = []state.Agent{{Party: original, Task: sshTestTask, AssignedAt: assignedAt}}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			runProblemTestCommand(t, a, "problem", midtaskFinding)
			if tc.idle {
				runProblemTestCommand(t, a, verbIdle, flagProblem, midtaskFinding)
			}
			a.now = a.now.Add(time.Second)
			nextParty := original
			live := func(state.Party) bool { return true }
			if tc.replaced {
				nextParty = state.Party{Name: agentOne, Session: "replacement-session"}
				live = func(p state.Party) bool { return !p.Is(original) }
			}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				watch.FirePending(st, nil, a.now)
				_, err := registerAgent(st, nextParty, live, a.now)
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
		lines, events := watch.FirePending(st, nil, a.now)
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
	lines, _ := watch.FirePending(st, nil, a.now)
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
	s := hostUpdateServer(t, a, hostNewRelease)
	request.PolicyDigest = s.Policy().Digest()
	j := execution.Journal{Request: request, Digest: request.Digest(), Handle: execution.Handle{Ref: request.Ref, Unit: execution.UnitName(request.Ref), ConversationID: request.ConversationID}}
	rawJournal, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, string(request.Ref.Execution)+".json"), rawJournal, 0o600); err != nil {
		t.Fatal(err)
	}
	s.OwnUnit = func() string { return j.Handle.Unit }
	drop := true
	s.Callback = func(ctx context.Context, _ []string, raw []byte) ([]byte, error) {
		var out bytes.Buffer
		if err := a.serveExecutionCoordinator(ctx, string(request.Ref.Host), bytes.NewReader(raw), &out); err != nil {
			return nil, err
		}
		if drop {
			drop = false
			return nil, errors.New("fixture reply lost after apply")
		}
		return out.Bytes(), nil
	}
	t.Setenv(execution.EnvID, string(request.Ref.Execution))
	runManagedCLI := func(args ...string) (string, error) {
		t.Helper()
		var out bytes.Buffer
		a.out = &out
		root := &cobra.Command{
			Use:           project.Name,
			SilenceUsage:  true,
			SilenceErrors: true,
			PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
				return a.checkExecutionCommand(cmd)
			},
		}
		root.AddCommand(a.agentsCmd())
		root.SetArgs(args)
		root.SetOut(&out)
		root.SetErr(&out)
		err := root.Execute()
		return out.String(), err
	}
	if _, err := runManagedCLI(agentsName, "assign", request.Name, sshTestTask); err == nil {
		t.Fatal("disallowed command accepted in managed execution worker")
	}
	rawFinding := strings.ReplaceAll(midtaskFinding, " ", "\n\t")
	if _, err := runManagedCLI(agentsName, "problem", rawFinding); err == nil {
		t.Fatal("lost reply reported success")
	}
	if out, err := runManagedCLI(agentsName, "problem", rawFinding); err != nil || !strings.Contains(out, "problem acknowledged by coordinator") {
		t.Fatalf("remote CLI problem failed: %v, out=%q", err, out)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		lines, events := watch.FirePending(st, nil, a.now)
		if len(lines) == 0 {
			t.Fatal("remote finding was not delivered")
		}
		return events, nil
	}); err != nil {
		t.Fatal(err)
	}
	var result execution.ControlResult
	control := execution.ControlRequest{Ref: request.Ref, ID: "report-after-problem", Sequence: 2, Operation: execution.ControlReport, Data: mustExecutionJSON(t, executionReport{Finished: true, Text: "fixture completed", Problems: []string{midtaskFinding}})}
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
