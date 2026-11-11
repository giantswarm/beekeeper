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

	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
	"github.com/giantswarm/beekeeper/pkg/project"
)

const sshTestOtherSelector = "host-b"

func sshStartTestApp(t *testing.T) (*app, string) {
	t.Helper()
	a, _ := stubApp(t)
	// Temporary roots may be aliases; managed storage requires a canonical
	// operator-owned path on every coordinator platform.
	root, err := filepath.EvalSymlinks(a.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.StateDir = root
	plat.Machine = roomyMachine{Machine: plat.Machine}
	a.cfg.Identity.Host = sshTestCoordinator
	a.cfg.Capacity = config.Capacity{Ceiling: 10, Providers: map[string]config.Provider{claudeComm: {Ceiling: 1}}, Choices: []config.Choice{{Harness: claudeComm, Model: bgModel}}}
	stubSubscriptionUsage(a)
	a.cfg.Agents.Hosts = map[string]config.ExecutionHost{}
	for _, selector := range []string{sshTestSelector, sshTestOtherSelector} {
		a.cfg.Agents.Hosts[selector] = execution.Host{Endpoint: execution.Endpoint{SSH: selector + ".example", Executable: sshTestExecutable, Config: sshTestConfig}, Identity: execution.HostID(selector), WorkspaceRoot: "/srv/workspaces", Repositories: map[string]string{sshTestRepository: "/srv/source"}, Harnesses: []string{claudeComm}, PolicyDigest: execution.Digest([]byte("fixture policy")), MaxBusy: 2}
	}
	a.executionProbe = func(_ context.Context, h execution.Host) (execution.Capabilities, error) {
		return execution.Capabilities{CIWait: true, SourcePreparation: true, Publication: true, Protocol: execution.Protocol, Identity: h.Identity, Version: project.Version(), PolicyDigest: h.PolicyDigest, Harnesses: map[string]string{claudeComm: "2.1.290"}, Resources: execution.Resources{ObservedAt: time.Now(), AvailableMiB: 1 << 16}}, nil
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
	withCoordinatorVersion(t, hostNewRelease)
	previous := coordinatorJournal
	coordinatorJournal = func() (*update.Journal, error) {
		return &update.Journal{Probation: coordinatorProbationPending, Candidate: update.Binary{Version: hostNewRelease}}, nil
	}
	t.Cleanup(func() { coordinatorJournal = previous })
	for _, interrupt := range []string{"", execution.Prepared, execution.Activating} {
		t.Run(interrupt, func(t *testing.T) {
			a, brief := sshStartTestApp(t)
			probe := a.executionProbe
			a.executionProbe = func(ctx context.Context, h execution.Host) (execution.Capabilities, error) {
				caps, err := probe(ctx, h)
				caps.Version = hostOldRelease
				return caps, err
			}
			var handle execution.Handle
			process, starts, activations, lost := execution.Prepared, 0, 0, false
			fake := &testExecutionTransport{}
			fake.start = func(_ context.Context, r execution.StartRequest) (execution.Handle, error) {
				starts++
				st, err := a.store.Read()
				if err != nil || len(st.Agents) != 1 || len(st.Starts) != 1 || st.Agents[0].Execution == nil || *st.Agents[0].Execution != r.Ref {
					t.Fatal("SSH start preceded durable roster registration")
				}
				if endpoint := st.Starts[0].Execution.Endpoint; endpoint == nil || *endpoint != a.cfg.Agents.Hosts[sshTestSelector].Endpoint {
					t.Fatal("SSH start preceded durable endpoint pin")
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
			// A lost activation reply settles by the host's journal: the process
			// runs, so the start took and nothing activates it twice.
			if interrupt == execution.Prepared {
				if err == nil {
					t.Fatal("interrupted start reported success")
				}
				st, readErr := a.store.Read()
				if readErr != nil || len(st.Starts) != 1 || len(st.Agents) != 1 || len(countAgents(st, nil, a.now, nil).Busy) != 1 {
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
			if busy := localBusy(st, []string{st.Agents[0].Name}); len(busy) != 0 {
				t.Fatalf("remote probation start used coordinator capacity: %v", busy)
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
	counts := countProviderAgents(st, nil, a.cfg.Capacity, a.now, nil)
	if len(counts[claudeComm].Busy) != 1 || st.Starts[0].Execution.Observation.Reachability != execution.Unreachable {
		t.Fatal("unreachable host released its provider reservation")
	}
}

func TestExecutionAdmissionKeepsTargetGuardsAndUnknownReservations(t *testing.T) {
	a, _ := sshStartTestApp(t)
	host := a.cfg.Agents.Hosts[sshTestSelector]
	resources := execution.Resources{ObservedAt: time.Now(), Blocks: []string{"fixture target memory pressure"}}
	v := a.executionCapacity(&state.State{}, nil, a.cfg.Capacity, host, resources, nil, nil, false, nil, forWorker, claudeComm, bgModel)
	if v.Room != 0 || !strings.Contains(strings.Join(v.Blocks, ";"), "target memory pressure") {
		t.Fatal("provider narrowing removed the target machine guard")
	}
	st := &state.State{Agents: []state.Agent{{Party: state.Party{Name: "unconfirmed-worker"}, Execution: &execution.Ref{Host: host.Identity, Execution: workerGitHubMissingRef, Generation: 1}}}}
	resources.Blocks = nil
	v = a.executionCapacity(st, nil, a.cfg.Capacity, host, resources, nil, nil, false, nil, forWorker, claudeComm, bgModel)
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
	counts := countProviderAgents(st, nil, a.cfg.Capacity, a.now, nil)
	if len(counts[claudeComm].Busy) != 1 || len(counts["other-pool"].Busy) != 0 {
		t.Fatal("configuration moved a live reservation to a fresh provider")
	}
	delete(a.cfg.Capacity.Providers, claudeComm)
	counts = countProviderAgents(st, nil, a.cfg.Capacity, a.now, nil)
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

// The serving-mode ceiling of a self-hosted provider stands during admission:
// readings must not rewrite the shared capacity map, or every start would
// refuse with "provider configuration changed during admission".
func TestExecutionAdmissionServeModeCeiling(t *testing.T) {
	a, brief := sshStartTestApp(t)
	const provLocal = "local-model"
	host := a.cfg.Agents.Hosts[sshTestSelector]
	raw := "capacity:\n" +
		"  ceiling: 10\n" +
		"  providers:\n" +
		"    " + claudeComm + ":\n" +
		"      ceiling: 5\n" +
		"    " + provLocal + ":\n" +
		"      kind: self-hosted\n" +
		"      ceiling: 5\n" +
		"      serveModeCommand: [read-serve-mode]\n" +
		"      serveModeCeilings:\n" +
		"        " + serveModeFull + ": 1\n" +
		"  choices:\n" +
		"    - harness: " + claudeComm + "\n" +
		"      model: " + bgModel + "\n" +
		"      provider: " + claudeComm + "\n" +
		"agents:\n" +
		"  hosts:\n" +
		"    " + sshTestSelector + ":\n" +
		"      ssh: " + host.SSH + "\n" +
		"      executable: " + host.Executable + "\n" +
		"      config: " + host.Config + "\n" +
		"      identity: " + string(host.Identity) + "\n" +
		"      workspaceRoot: " + host.WorkspaceRoot + "\n" +
		"      repositories:\n" +
		"        " + sshTestRepository + ": " + host.Repositories[sshTestRepository] + "\n" +
		"      harnesses: [" + claudeComm + "]\n" +
		"      policyDigest: " + host.PolicyDigest + "\n" +
		"      maxBusy: 2\n"
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	full := "stateDir: " + a.cfg.StateDir + "\nleaseDir: " + a.cfg.LeaseDir + "\nmemcap: {slotDir: " + filepath.Join(dir, "slots") + "}\n" +
		"claude: {projectsDir: " + filepath.Join(dir, "projects") + ", sessionsDir: " + filepath.Join(dir, "sessions") + "}\n" + raw
	if err := os.WriteFile(cfgPath, []byte(full), 0o600); err != nil {
		t.Fatal(err)
	}
	// The fixture rewrote the capacity in memory; the disk config is the
	// truth for both sides of the admission re-check.
	cfg2, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	a.cfgPath = cfgPath
	a.cfg.Capacity = cfg2.Capacity
	a.readUsage = func(context.Context, []string, time.Duration) (usageReading, error) {
		return usageReading{FiveHour: &usageWindow{}, SevenDay: &usageWindow{}}, nil
	}
	modeReads := 0
	a.readServeMode = func(context.Context, []string, time.Duration) (config.ServeModeReading, error) {
		modeReads++
		return config.ServeModeReading{Mode: serveModeFull}, nil
	}
	fake := &testExecutionTransport{}
	var handle execution.Handle
	fake.start = func(_ context.Context, r execution.StartRequest) (execution.Handle, error) {
		handle = execution.Handle{Ref: r.Ref, Repository: r.Repository, Commit: strings.Repeat("a", 40), Workspace: path.Join(a.cfg.Agents.Hosts[sshTestSelector].WorkspaceRoot, string(r.Ref.Execution), "workspace"), Unit: execution.UnitName(r.Ref), ConversationID: r.ConversationID}
		return handle, nil
	}
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		return execution.Observation{Handle: handle, ProcessState: execution.Running, Ready: true}, nil
	}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	if err := a.startSSHExecution(context.Background(), "worker-serve", brief, executionStartOptions{Host: sshTestSelector, Repository: sshTestRepository, GitRef: mainBranch, Harness: claudeComm, Model: bgModel}); err != nil {
		t.Fatalf("serve-mode ceiling broke admission: %v", err)
	}
	if modeReads == 0 {
		t.Fatal("the serving mode of the self-hosted provider was never read")
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Starts) != 1 {
		t.Fatalf("admission reserved %d executions, want 1", len(st.Starts))
	}
	// The shared config keeps the configured ceiling: reading the serving
	// mode never mutated it, so the admission re-check cannot fire.
	if got := a.cfg.Capacity.Providers[provLocal].Ceiling; got != 5 {
		t.Fatalf("configured ceiling mutated to %d, want 5", got)
	}
	if err := os.Remove(filepath.Join(a.store.Dir(), serveModeFile(provLocal))); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	a.executionReadings(context.Background(), a.liveCapacity())
	if got := a.cfg.Capacity.Providers[provLocal].Ceiling; got != 5 {
		t.Fatalf("executionReadings mutated the configured ceiling to %d, want 5", got)
	}
}

// A host's verdict weighs only the choices it can run: an omp choice with
// room is no room on a Claude-only host, so the loop never places a start
// there that the host's start would refuse.
func TestExecutionCapacityWeighsOnlyTheHostsHarnesses(t *testing.T) {
	a, _ := sshStartTestApp(t)
	host := a.cfg.Agents.Hosts[sshTestSelector]
	// A self-hosted omp choice with room of its own: locally it would start.
	k := config.Capacity{Ceiling: 10, Providers: map[string]config.Provider{"local": {Kind: config.KindSelfHosted, Ceiling: 2}}, Choices: []config.Choice{{Harness: omp.Harness, Model: "local/fixture", Provider: "local", Kinds: []string{workerRole}}}}
	if v := buildProvidersCapacity(agentCount{}, nil, k, nil, roomy(), false, nil, forWorker, a.now); v.Room == 0 {
		t.Fatalf("fixture: the omp choice has no room locally either: %v", v.Blocks)
	}
	resources := execution.Resources{ObservedAt: time.Now(), AvailableMiB: 1 << 16}
	if v := a.executionCapacity(&state.State{}, nil, k, host, resources, nil, nil, false, nil, forWorker, harnessAuto, ""); v.Room != 0 || (v.Choice != nil && v.Choice.Harness != "") {
		t.Fatalf("a Claude-only host offers an omp choice: room %d, choice %+v", v.Room, v.Choice)
	}
	k = a.cfg.Capacity
	k.Choices = append([]config.Choice{{Harness: codex.Harness, Model: "gpt-fixture"}}, k.Choices...)
	if v := a.executionCapacity(&state.State{}, nil, k, host, resources, a.executionReadings(context.Background(), k), nil, false, nil, forWorker, harnessAuto, ""); v.Room == 0 || v.Choice == nil || v.Choice.Harness != claudeComm {
		t.Fatalf("the host's Claude choice: room %d, choice %+v, blocks %v", v.Room, v.Choice, v.Blocks)
	}
}

// The original coordinator prepared the host but lost its reply before source
// transfer existed. The upgraded transport offers uploads; recovery must still
// use the original journal/request, without fetching or uploading new source.
func TestExecutionUpgradeRecoversLegacyLostPreparationReply(t *testing.T) {
	a, brief := sshStartTestApp(t)
	var original execution.StartRequest
	var handle execution.Handle
	prepared, running, activations, uploads, fetches := false, false, 0, 0, 0
	fake := &testExecutionTransport{}
	fake.start = func(_ context.Context, r execution.StartRequest) (execution.Handle, error) {
		if !prepared {
			original = r
			handle = execution.Handle{Ref: r.Ref, Repository: r.Repository, Commit: strings.Repeat("a", 40), Workspace: path.Join(a.cfg.Agents.Hosts[sshTestSelector].WorkspaceRoot, string(r.Ref.Execution), "workspace"), Unit: execution.UnitName(r.Ref), ConversationID: r.ConversationID}
			prepared = true
			return execution.Handle{}, &execution.UnreachableError{Operation: agentStartName, Cause: errors.New("lost legacy preparation reply")}
		}
		if r.Digest() != original.Digest() {
			return execution.Handle{}, errors.New("legacy journal request digest changed")
		}
		if r.RegistrationReceipt != "" {
			activations++
			running = true
		}
		return handle, nil
	}
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		phase := execution.Prepared
		if running {
			phase = execution.Running
		}
		return execution.Observation{Handle: handle, ProcessState: phase, Ready: running}, nil
	}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	if _, err := runAgents(a, agentStartName, "--name", "upgrade-worker", brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--model", bgModel); err == nil {
		t.Fatal("lost preparation reply succeeded")
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := executionRecord(st, original.Ref)
	if rec == nil || rec.Phase != executionPending || rec.Request.SourceDigest != "" {
		t.Fatal("fixture is not a pending legacy preparation")
	}
	gitBefore := executionPublishGit
	t.Cleanup(func() { executionPublishGit = gitBefore })
	executionPublishGit = func(context.Context, string, ...string) ([]byte, error) {
		fetches++
		return nil, errors.New("legacy recovery must not fetch source")
	}
	upgraded := &sourceRecoveryTransport{testExecutionTransport: fake, upload: func(context.Context, execution.Ref, []byte) error {
		uploads++
		return errors.New("legacy journal has no source bundle")
	}}
	a.executionTransport = func(execution.Host) execution.Transport { return upgraded }
	inspectBefore := fake.inspect
	for _, failure := range []error{errors.New("unreadable legacy journal"), &execution.UnreachableError{Operation: sshTestInspect, Cause: errors.New("connection lost")}} {
		fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
			return execution.Observation{}, failure
		}
		if err := a.activateExecution(context.Background(), original.Ref); !errors.Is(err, failure) {
			t.Fatalf("inspection error was not preserved: %v", err)
		}
		st, err = a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		rec, _ = executionRecord(st, original.Ref)
		if fetches != 0 || uploads != 0 || (rec.Request.Digest() != original.Digest() || rec.RequestDigest != original.Digest()) {
			t.Fatal("uncertain inspection changed the legacy request")
		}
	}
	fake.inspect = inspectBefore
	if err := a.activateExecution(context.Background(), original.Ref); err != nil {
		t.Fatalf("upgraded recovery: %v", err)
	}
	st, err = a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	rec, _ = executionRecord(st, original.Ref)
	if fetches != 0 || uploads != 0 || activations != 1 || (rec.Request.Digest() != original.Digest() || rec.RequestDigest != original.Digest()) || !rec.Observation.Ready {
		t.Fatal("recovery changed the legacy request, uploaded source, or duplicated activation")
	}
}

type sourceRecoveryTransport struct {
	*testExecutionTransport
	upload func(context.Context, execution.Ref, []byte) error
}

func (t *sourceRecoveryTransport) UploadSource(ctx context.Context, ref execution.Ref, data []byte) error {
	return t.upload(ctx, ref, data)
}

func TestExecutionParkedWorkerCountsParkedNotBusy(t *testing.T) {
	a, brief := sshStartTestApp(t)
	fake := &testExecutionTransport{}
	var handle execution.Handle
	fake.start = func(_ context.Context, r execution.StartRequest) (execution.Handle, error) {
		handle = execution.Handle{Ref: r.Ref, Repository: r.Repository, Commit: strings.Repeat("a", 40), Workspace: path.Join(a.cfg.Agents.Hosts[sshTestSelector].WorkspaceRoot, string(r.Ref.Execution), "workspace"), Unit: execution.UnitName(r.Ref), ConversationID: r.ConversationID}
		return handle, nil
	}
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		return execution.Observation{Handle: handle, ProcessState: execution.Running, Ready: true}, nil
	}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	if err := a.startSSHExecution(context.Background(), "worker-parked", brief, executionStartOptions{Host: sshTestSelector, Repository: sshTestRepository, GitRef: mainBranch, Harness: claudeComm, Model: bgModel}); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	hostID := a.cfg.Agents.Hosts[sshTestSelector].Identity
	if got := countAgents(st, nil, a.now, a.cfg); len(got.Busy) != 1 || len(got.Parked) != 0 {
		t.Fatalf("before park countAgents = %+v, want Busy=1 Parked=0", got)
	}
	if got := executionHostBusy(st, hostID); got != 1 {
		t.Fatalf("before park executionHostBusy = %d, want 1", got)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park = &state.Park{On: sshTestRepository + "#7", At: a.now, Waits: deskReviewWait, ReviewHead: strings.Repeat("a", 40)}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err = a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := countAgents(st, nil, a.now, a.cfg); len(got.Busy) != 0 || len(got.Parked) != 1 {
		t.Fatalf("after park countAgents = %+v, want Busy=0 Parked=1", got)
	}
	if got := executionHostBusy(st, hostID); got != 0 {
		t.Fatalf("after park executionHostBusy = %d, want 0", got)
	}
	k := config.Capacity{
		Providers: map[string]config.Provider{claudeComm: {Kind: config.KindSubscription, Ceiling: 2}},
		Choices:   []config.Choice{{Harness: claudeComm, Model: bgModel, Provider: claudeComm}},
	}
	if got := countProviderAgents(st, nil, k, a.now, a.cfg)[claudeComm]; len(got.Busy) != 0 {
		t.Fatalf("after park countProviderAgents = %+v, want Busy=0", got)
	}
}

func TestSSHStartAdmitsOrganizationRepositoryWithoutMapping(t *testing.T) {
	a, brief := sshStartTestApp(t)
	h := a.cfg.Agents.Hosts[sshTestSelector]
	h.RepositoryOrgs = []string{"example"}
	a.cfg.Agents.Hosts[sshTestSelector] = h
	out, err := runAgents(a, agentStartName, "--name", "third-repo-worker", brief, "--host", sshTestSelector, "--repo", "example/third", "--ref", mainBranch, "--model", bgModel, "--dry-run")
	if err != nil || !strings.Contains(out, "example/third") {
		t.Fatalf("organization repository dry run: %s %v", out, err)
	}
}
