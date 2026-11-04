package cmd

import (
	"context"
	"testing"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestRetainedExecutionRefusesCatalogRouteChange(t *testing.T) {
	for _, change := range []string{"ssh", "executable", "config", "identity", "alias", "exited"} {
		t.Run(change, func(t *testing.T) {
			a, _ := sshStartTestApp(t)
			original := a.cfg.Agents.Hosts[sshTestSelector]
			ref := execution.Ref{Host: original.Identity, Execution: "retained-route", Generation: 1}
			endpoint := original.Endpoint
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				phase := execution.Running
				if change == "exited" {
					phase = execution.Exited
				}
				st.Starts = append(st.Starts, state.Start{Execution: &state.ExecutionRecord{Handle: execution.Handle{Ref: ref}, Selector: sshTestSelector, Endpoint: &endpoint, Phase: phase, Observation: execution.Observation{ProcessState: phase}}})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			if change != "exited" {
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
			case "ssh":
				changed.SSH = "replacement.example"
			case "executable":
				changed.Executable = "/opt/beekeeper"
			case "identity":
				changed.Identity = sshTestOtherSelector
			case "alias":
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
			if change != "alias" {
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
	for _, change := range []string{"policy", "alias", "other-host", "legacy"} {
		t.Run(change, func(t *testing.T) {
			a, _ := sshStartTestApp(t)
			original := a.cfg.Agents.Hosts[sshTestSelector]
			endpoint := original.Endpoint
			record := &state.ExecutionRecord{
				Handle:   execution.Handle{Ref: execution.Ref{Host: original.Identity, Execution: "compatible-route", Generation: 1}},
				Selector: sshTestSelector, Endpoint: &endpoint,
			}
			if change == "legacy" {
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
			case "policy":
				changed.PolicyDigest = execution.Digest([]byte("updated policy"))
			case "alias":
				selector = sshTestOtherSelector
			case "other-host":
				selector = sshTestOtherSelector
				changed = a.cfg.Agents.Hosts[selector]
			case "legacy":
				changed.Config = "/etc/beekeeper/legacy.yaml"
			}
			a.cfg.Agents.Hosts[selector] = changed
			if _, err := a.catalogHost(selector); err != nil {
				t.Fatalf("compatible catalog change refused: %v", err)
			}
		})
	}
}
