package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/project"
)

const sshTestOtherSelector = "host-b"

func sshStartTestApp(t *testing.T) (*app, string) {
	t.Helper()
	a, _ := stubApp(t)
	plat.Machine = roomyMachine{Machine: plat.Machine}
	a.cfg.Identity.Host = "coordinator"
	a.cfg.Capacity = config.Capacity{Ceiling: 10, Providers: map[string]config.Provider{claudeComm: {Ceiling: 1}}, Choices: []config.Choice{{Harness: claudeComm, Model: bgModel}}}
	a.cfg.Agents.Hosts = map[string]config.ExecutionHost{}
	for _, selector := range []string{sshTestSelector, sshTestOtherSelector} {
		a.cfg.Agents.Hosts[selector] = execution.Host{Endpoint: execution.Endpoint{SSH: selector + ".example", Executable: sshTestExecutable, Config: sshTestConfig}, Identity: execution.HostID(selector), WorkspaceRoot: "/srv/workspaces", Repositories: map[string]string{sshTestRepository: "/srv/source"}, Harnesses: []string{claudeComm}, PolicyDigest: execution.Digest([]byte("fixture policy")), MaxBusy: 2}
	}
	a.executionProbe = func(_ context.Context, h execution.Host) (execution.Capabilities, error) {
		return execution.Capabilities{Protocol: execution.Protocol, Identity: h.Identity, Version: project.Version(), PolicyDigest: h.PolicyDigest, Harnesses: map[string]string{claudeComm: "2.1.290"}, Resources: execution.Resources{ObservedAt: time.Now(), AvailableMiB: 1 << 16}}, nil
	}
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte(sshTestTask), 0o600); err != nil {
		t.Fatal(err)
	}
	return a, brief
}

// A lost activation reply must leave exactly one registered execution. Its
// recovery uses the same reference and observes the already-running process.
func TestExecutionStartThroughHarnessRegistersBeforeSSHAndRecoversLostReply(t *testing.T) {
	for _, interrupt := range []string{"", execution.Prepared, execution.Activating} {
		t.Run(interrupt, func(t *testing.T) {
			a, brief := sshStartTestApp(t)
			var handle execution.Handle
			process, starts, activations, lost := execution.Prepared, 0, 0, false
			fake := &testExecutionTransport{}
			fake.start = func(_ context.Context, r execution.StartRequest) (execution.Handle, error) {
				starts++
				st, err := a.store.Read()
				if err != nil || len(st.Agents) != 1 || len(st.Starts) != 1 || st.Agents[0].Execution == nil || *st.Agents[0].Execution != r.Ref {
					t.Fatal("SSH start preceded durable roster registration")
				}
				if handle.Ref.Execution != "" && handle.Ref != r.Ref {
					t.Fatal("recovery allocated another execution")
				}
				handle = execution.Handle{Ref: r.Ref, Repository: r.Repository, Commit: strings.Repeat("a", 40), Workspace: path.Join(a.cfg.Agents.Hosts[sshTestSelector].WorkspaceRoot, string(r.Ref.Execution), "workspace"), Unit: execution.UnitName(r.Ref), ConversationID: r.ConversationID}
				if r.RegistrationReceipt != "" {
					activations++
					process = execution.Running
				}
				if !lost && (interrupt == execution.Prepared && r.RegistrationReceipt == "" || interrupt == execution.Activating && r.RegistrationReceipt != "") {
					lost = true
					return execution.Handle{}, &execution.UnreachableError{Operation: agentStartName, Cause: errors.New("fixture reply lost")}
				}
				return handle, nil
			}
			fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
				return execution.Observation{Handle: handle, ProcessState: process, Ready: process == execution.Running}, nil
			}
			a.executionTransport = func(execution.Host) execution.Transport { return fake }
			out, err := runAgents(a, agentStartName, "--name", "remote-worker", brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--model", bgModel)
			if interrupt != "" {
				if err == nil {
					t.Fatal("interrupted start reported success")
				}
				st, readErr := a.store.Read()
				if readErr != nil || len(st.Starts) != 1 || len(st.Agents) != 1 || len(countAgents(st, nil, a.now).Busy) != 1 {
					t.Fatal("interruption dropped the reservation")
				}
				if err = a.activateExecution(context.Background(), *st.Agents[0].Execution); err != nil {
					t.Fatal(err)
				}
			} else if err != nil || !strings.Contains(out, "workspace") || !strings.Contains(out, "conversation") {
				t.Fatalf("start: %s, %v", out, err)
			}
			st, err := a.store.Read()
			if err != nil || len(st.Starts) != 1 || len(st.Agents) != 1 || activations != 1 || starts > 3 || !st.Starts[0].Execution.Observation.Ready {
				t.Fatal("recovery duplicated registration/activation or lost acceptance")
			}
		})
	}
}

// Both hosts probe before either caller can reserve. Admission must recompute
// shared subscription counts under the file-backed state lock.
func TestExecutionTwoHostsCompeteForFinalProviderSlot(t *testing.T) {
	a, brief := sshStartTestApp(t)
	a.out = io.Discard
	probe := a.executionProbe
	var ready sync.WaitGroup
	ready.Add(2)
	a.executionProbe = func(ctx context.Context, h execution.Host) (execution.Capabilities, error) {
		ready.Done()
		ready.Wait()
		return probe(ctx, h)
	}
	var mu sync.Mutex
	preparations := 0
	a.executionTransport = func(execution.Host) execution.Transport {
		return &testExecutionTransport{start: func(context.Context, execution.StartRequest) (execution.Handle, error) {
			mu.Lock()
			preparations++
			mu.Unlock()
			return execution.Handle{}, &execution.UnreachableError{Operation: agentStartName, Cause: errors.New("fixture disconnection")}
		}}
	}
	results := make(chan error, 2)
	for _, selector := range []string{sshTestSelector, sshTestOtherSelector} {
		go func() {
			results <- a.startSSHExecution(context.Background(), "worker-"+selector, brief, executionStartOptions{Host: selector, Repository: sshTestRepository, GitRef: mainBranch, Harness: claudeComm, Model: bgModel})
		}()
	}
	for range 2 {
		if err := <-results; err == nil {
			t.Fatal("disconnected start reported success")
		}
	}
	st, err := a.store.Read()
	if err != nil || len(st.Agents) != 1 || len(st.Starts) != 1 || preparations != 1 {
		t.Fatalf("provider reservation raced: %d preparations, %v", preparations, err)
	}
	counts := countProviderAgents(st, nil, a.cfg.Capacity, a.now)
	if len(counts[claudeComm].Busy) != 1 || st.Starts[0].Execution.Observation.Reachability != execution.Unreachable {
		t.Fatal("unreachable host released its provider reservation")
	}
}

func TestExecutionAdmissionKeepsTargetGuardsAndUnknownReservations(t *testing.T) {
	a, _ := sshStartTestApp(t)
	host := a.cfg.Agents.Hosts[sshTestSelector]
	resources := execution.Resources{ObservedAt: time.Now(), Blocks: []string{"fixture target memory pressure"}}
	v := a.executionCapacity(&state.State{}, nil, a.cfg.Capacity, host, resources, nil, nil, false, nil, false, claudeComm, bgModel)
	if v.Room != 0 || !strings.Contains(strings.Join(v.Blocks, ";"), "target memory pressure") {
		t.Fatal("provider narrowing removed the target machine guard")
	}
	st := &state.State{Agents: []state.Agent{{Party: state.Party{Name: "unconfirmed-worker"}, Execution: &execution.Ref{Host: host.Identity, Execution: "missing", Generation: 1}}}}
	resources.Blocks = nil
	v = a.executionCapacity(st, nil, a.cfg.Capacity, host, resources, nil, nil, false, nil, false, claudeComm, bgModel)
	if v.Room != 0 || len(v.Providers[claudeComm].Busy) != 1 || executionHostBusy(st, host.Identity) != 1 {
		t.Fatal("missing execution record became free capacity")
	}
}

func TestExecutionProviderReservationStaysPinnedAcrossConfigurationChanges(t *testing.T) {
	a, _ := sshStartTestApp(t)
	ref := execution.Ref{Host: sshTestSelector, Execution: "fixture", Generation: 1}
	st := &state.State{Agents: []state.Agent{{Party: state.Party{Name: "remote-worker"}, Execution: &ref, Harness: claudeComm, Model: bgModel, Done: true}}, Starts: []state.Start{{Execution: &state.ExecutionRecord{Handle: execution.Handle{Ref: ref}, Provider: claudeComm, Observation: execution.Observation{ProcessState: execution.Running, Reachability: execution.Unreachable}}}}}
	a.cfg.Capacity.Providers["other-pool"] = config.Provider{Ceiling: 2}
	a.cfg.Capacity.Choices[0].Provider = "other-pool"
	counts := countProviderAgents(st, nil, a.cfg.Capacity, a.now)
	if len(counts[claudeComm].Busy) != 1 || len(counts["other-pool"].Busy) != 0 {
		t.Fatal("configuration moved a live reservation to a fresh provider")
	}
	delete(a.cfg.Capacity.Providers, claudeComm)
	counts = countProviderAgents(st, nil, a.cfg.Capacity, a.now)
	if len(counts["other-pool"].Unconfirmed) != 1 {
		t.Fatal("removing the pinned provider released its reservation")
	}
}

func TestExecutionStatusUsesContactTimeSeparatelyFromProcessEvidence(t *testing.T) {
	a, r := executionTestApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		o := &st.Starts[0].Execution.Observation
		o.ObservedAt = a.now.Add(-time.Hour)
		o.ReachabilityAt = a.now
		o.Reachability = execution.Unreachable
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	views := a.agentViews(st, nil)
	if len(views) != 1 || views[0].Reachable != "SSH unreachable; process running" || views[0].Task != r.Task {
		t.Fatal("status confused contact time with process evidence or task state")
	}
}

func TestExecutionAutoModelValidationDryRunAndLocalSelection(t *testing.T) {
	a, brief := sshStartTestApp(t)
	baseProbe := a.executionProbe
	probes := 0
	a.executionProbe = func(ctx context.Context, host execution.Host) (execution.Capabilities, error) {
		probes++
		return baseProbe(ctx, host)
	}
	a.executionTransport = func(execution.Host) execution.Transport {
		t.Fatal("dry-run started a remote process")
		return nil
	}
	args := []string{agentStartName, workerRole, brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--harness", harnessAuto, testDryRunFlag}
	if _, err := runAgents(a, append(args, "--model", "unconfigured-model")...); Code(err) != ExitUsage || probes != 0 {
		t.Fatalf("unknown auto model touched transport: %v, probes %d", err, probes)
	}
	if out, err := runAgents(a, append(args, "--model", "OPUS")...); err != nil || !strings.Contains(out, "on opus") {
		t.Fatalf("auto model filter: %s, %v", out, err)
	}
	st, err := a.store.Read()
	if err != nil || len(st.Starts) != 0 || len(st.Agents) != 0 {
		t.Fatal("dry-run changed the roster")
	}
	a.out.(*bytes.Buffer).Reset()
	if _, err := runAgents(a, agentStartName, workerRole, brief, "--host", executionLocal, "--repo", sshTestRepository); Code(err) != ExitUsage || probes != 1 {
		t.Fatal("local selector used an SSH path")
	}
}
