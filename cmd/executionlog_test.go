package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestExecutionLogServesAWorkerItsProof(t *testing.T) {
	a, r := executionTestApp(t)
	a.cfg.Identity.Host = r.Coordinator
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		return []state.Event{
			event(state.Party{Name: "supervisor"}, "fixture.rolled", "release rolled onto the coordinator"),
			event(state.Party{Name: "supervisor"}, "fixture.other", "unrelated event"),
		}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var result executionLogResult
	if err := coordinatorTestCall(t, a, r, executionLogRead, executionLogRequest{Lines: 10, Verb: "fixture.rolled"}, &result, false); err != nil {
		t.Fatal(err)
	}
	if result.Code != 0 || !strings.Contains(result.Output, "release rolled onto the coordinator") || strings.Contains(result.Output, "unrelated event") {
		t.Fatalf("log read = %+v", result)
	}

	// A bad flag comes back as the log's own error, not as a transport fault.
	if err := coordinatorTestCall(t, a, r, executionLogRead, executionLogRequest{DefaultLines: true, Since: "not a time"}, &result, false); err != nil {
		t.Fatal(err)
	}
	if result.Code == 0 || result.Error == "" {
		t.Fatalf("invalid window = %+v", result)
	}

	// The window is bounded, the newest events kept.
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		evs := []state.Event{event(state.Party{Name: "supervisor"}, "fixture.old", "oldest event")}
		for range 4 {
			evs = append(evs, event(state.Party{Name: "supervisor"}, "fixture.bulk", "%s", strings.Repeat("x", executionLogLimit/2)))
		}
		return append(evs, event(state.Party{Name: "supervisor"}, "fixture.new", "newest event")), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := coordinatorTestCall(t, a, r, executionLogRead, executionLogRequest{Lines: 0, Verb: "fixture."}, &result, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Output, "newest event") || strings.Contains(result.Output, "oldest event") || !strings.Contains(result.Output, "older events cut") {
		t.Fatalf("bounded read lost its tail or its note, %d bytes", len(result.Output))
	}
}

func TestExecutionLogRefusesAnEndedExecution(t *testing.T) {
	a, r := executionTestApp(t)
	a.cfg.Identity.Host = r.Coordinator
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Completed = true
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	var result executionLogResult
	err := coordinatorTestCall(t, a, r, executionLogRead, executionLogRequest{Lines: 1}, &result, false)
	if err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Fatalf("ended execution read the log: %v", err)
	}
}

// The budget is the coordinator's: an execution without a scoped credential
// reads it, and the access an ended one lost names why.
func TestWorkerBudgetNeedsALiveExecutionNotAGrant(t *testing.T) {
	a, r := executionTestApp(t)
	a.cfg.Identity.Host = r.Coordinator
	var result workerBudgetResult
	if err := coordinatorTestCall(t, a, r, workerGitHubBudget, workerBudgetRequest{Gate: true}, &result, false); err != nil {
		t.Fatalf("a worker without a grant could not read the budget: %v", err)
	}
	if strings.Contains(result.Error, "revoked") {
		t.Fatalf("budget = %+v", result)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.StopRequest = &execution.StopRequest{Reason: "fixture"}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	err := coordinatorTestCall(t, a, r, workerGitHubBudget, workerBudgetRequest{Gate: true}, &result, false)
	if err == nil || !strings.Contains(err.Error(), "revoked: the execution has a stop request") {
		t.Fatalf("stopped execution: %v", err)
	}
}

// Without a grant a credential is refused for what it is, never as "revoked".
func TestWorkerGitHubNamesAMissingGrant(t *testing.T) {
	a, r := executionTestApp(t)
	a.cfg.Identity.Host = r.Coordinator
	err := a.workerGitHubLive(r.Ref, workerGitHubRequest{Repository: r.Repository})
	if err == nil || strings.Contains(err.Error(), "revoked") || !strings.Contains(err.Error(), "github.workerApp") {
		t.Fatalf("no grant = %v", err)
	}
}

// hosts check and agents start answer for a worker's GitHub reads: a host with
// neither the worker App nor its own gh login fails them, naming the reason. An
// older host, which does not say, keeps working until it updates.
func TestExecutionCheckAnswersForWorkerGitHubReads(t *testing.T) {
	for name, tc := range map[string]struct {
		login, wantErr string
		app            bool
	}{
		"logged out":        {login: execution.GitHubLoggedOut, wantErr: "gives its workers no GitHub reads"},
		"host login":        {login: execution.GitHubLoggedIn},
		"worker app":        {login: execution.GitHubLoggedOut, app: true},
		"host says nothing": {},
	} {
		t.Run(name, func(t *testing.T) {
			a, r := executionTestApp(t)
			host := a.cfg.Agents.Hosts[sshTestSelector]
			if tc.app {
				a.cfg.GitHub.WorkerApp = &config.WorkerGitHub{}
			}
			a.executionProbe = func(context.Context, execution.Host) (execution.Capabilities, error) {
				return execution.Capabilities{CIWait: true, SourcePreparation: true, GitHub: tc.app, GitHubLogin: tc.login, Publication: true, Protocol: execution.Protocol, Identity: r.Ref.Host, PolicyDigest: host.PolicyDigest,
					Harnesses: map[string]string{execution.HarnessClaude: publicationClaudeVersion}, Resources: execution.Resources{ObservedAt: time.Now(), AvailableMiB: 1 << 16}}, nil
			}
			caps, err := a.probeExecution(t.Context(), host)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || caps.GitHub {
					t.Fatalf("check = %+v, %v", caps.GitHub, err)
				}
				return
			}
			if err != nil || !caps.GitHub {
				t.Fatalf("check = %+v, %v", caps.GitHub, err)
			}
		})
	}
}
