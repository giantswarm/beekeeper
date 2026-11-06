package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	routeChangeAlias  = "route alias"
	routeChangeExited = "exited record"
	routeChangeLegacy = "legacy record"
)

const optBeekeeper = "/opt/beekeeper"

const otherHostName = "other-host"

const conflictingWord = "conflicting"

func TestRetainedExecutionRefusesCatalogRouteChange(t *testing.T) {
	for _, change := range []string{"SSH destination", "remote executable", "configuration path", "host identity", routeChangeAlias, routeChangeExited} {
		t.Run(change, func(t *testing.T) {
			a, _ := sshStartTestApp(t)
			original := a.cfg.Agents.Hosts[sshTestSelector]
			ref := execution.Ref{Host: original.Identity, Execution: "retained-route", Generation: 1}
			endpoint := original.Endpoint
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				phase := execution.Running
				if change == routeChangeExited {
					phase = execution.Exited
				}
				st.Starts = append(st.Starts, state.Start{Execution: &state.ExecutionRecord{Handle: execution.Handle{Ref: ref}, Selector: sshTestSelector, Endpoint: &endpoint, Phase: phase, Observation: execution.Observation{ProcessState: phase}}})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			if change != routeChangeExited {
				st, err := a.store.Read()
				if err != nil {
					t.Fatal(err)
				}
				if got := executionHostBusy(st, original.Identity); got != 1 {
					t.Fatalf("physical host reservations = %d, want 1", got)
				}
			}
			changed := original
			selector := sshTestSelector
			switch change {
			case "SSH destination":
				changed.SSH = "replacement.example"
			case "remote executable":
				changed.Executable = optBeekeeper
			case "host identity":
				changed.Identity = sshTestOtherSelector
			case routeChangeAlias:
				selector = sshTestOtherSelector
				changed.Config = "/etc/beekeeper/isolated.yaml"
			default:
				changed.Config = "/etc/beekeeper/isolated.yaml"
			}
			a.cfg.Agents.Hosts[selector] = changed
			a.executionTransport = func(execution.Host) execution.Transport {
				t.Fatal("changed route reached SSH transport")
				return nil
			}
			if _, err := a.catalogHost(selector); Code(err) != ExitRefused {
				t.Fatalf("catalog route change: %v", err)
			}
			if change != routeChangeAlias {
				if _, err := a.inspectExecution(context.Background(), ref); Code(err) != ExitRefused {
					t.Fatalf("inspection route change: %v", err)
				}
			}
			a.cfg.Agents.Hosts[selector] = original
			if _, err := a.executionHost(selector); err != nil {
				t.Fatalf("restored route refused: %v", err)
			}
		})
	}
}

func TestRetainedExecutionAllowsCompatibleCatalogChanges(t *testing.T) {
	for _, change := range []string{"policy digest", routeChangeAlias, otherHostName, routeChangeLegacy} {
		t.Run(change, func(t *testing.T) {
			a, _ := sshStartTestApp(t)
			original := a.cfg.Agents.Hosts[sshTestSelector]
			endpoint := original.Endpoint
			record := &state.ExecutionRecord{
				Handle:   execution.Handle{Ref: execution.Ref{Host: original.Identity, Execution: "compatible-route", Generation: 1}},
				Selector: sshTestSelector, Endpoint: &endpoint,
			}
			if change == routeChangeLegacy {
				record.Endpoint = nil
			}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Starts = append(st.Starts, state.Start{Execution: record})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			selector := sshTestSelector
			changed := original
			switch change {
			case "policy digest":
				changed.PolicyDigest = execution.Digest([]byte("updated policy"))
			case routeChangeAlias:
				selector = sshTestOtherSelector
			case otherHostName:
				selector = sshTestOtherSelector
				changed = a.cfg.Agents.Hosts[selector]
			case routeChangeLegacy:
				changed.Config = "/etc/beekeeper/legacy.yaml"
			}
			a.cfg.Agents.Hosts[selector] = changed
			if _, err := a.catalogHost(selector); err != nil {
				t.Fatalf("compatible catalog change refused: %v", err)
			}
		})
	}
}

// A second coordinator may finish reserving while this start probes its host.
// Its pin must be checked again under the reservation lock.
func TestRetainedExecutionRouteRecheckedAtAdmission(t *testing.T) {
	for _, sameRoute := range []bool{false, true} {
		t.Run(map[bool]string{false: conflictingWord, true: "compatible"}[sameRoute], func(t *testing.T) {
			a, brief := sshStartTestApp(t)
			probe := a.executionProbe
			a.executionProbe = func(ctx context.Context, h execution.Host) (execution.Capabilities, error) {
				endpoint := h.Endpoint
				if !sameRoute {
					endpoint.Config = "/etc/beekeeper/other.yaml"
				}
				if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.Starts = append(st.Starts, state.Start{Execution: &state.ExecutionRecord{
						Handle:   execution.Handle{Ref: execution.Ref{Host: h.Identity, Execution: "concurrent-route", Generation: 1}},
						Selector: sshTestOtherSelector, Endpoint: &endpoint,
						Observation: execution.Observation{ProcessState: execution.Exited},
					}})
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
				return probe(ctx, h)
			}
			preparations := 0
			a.executionTransport = func(execution.Host) execution.Transport {
				return &testExecutionTransport{start: func(context.Context, execution.StartRequest) (execution.Handle, error) {
					preparations++
					return execution.Handle{}, &execution.UnreachableError{Operation: agentStartName, Cause: refused("fixture ends after reservation")}
				}}
			}
			err := a.startSSHExecution(context.Background(), "route-worker", brief, executionStartOptions{Host: sshTestSelector, Repository: sshTestRepository, GitRef: mainBranch, Harness: claudeComm, Model: bgModel})
			st, readErr := a.store.Read()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if sameRoute {
				if len(st.Starts) != 2 || preparations != 1 {
					t.Fatalf("compatible reservation: starts=%d preparations=%d err=%v", len(st.Starts), preparations, err)
				}
			} else if Code(err) != ExitRefused || !strings.Contains(err.Error(), "route changed") || len(st.Starts) != 1 || preparations != 0 {
				t.Fatalf("conflicting reservation: starts=%d preparations=%d err=%v", len(st.Starts), preparations, err)
			}
		})
	}
}
