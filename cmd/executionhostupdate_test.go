package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
	"github.com/giantswarm/beekeeper/internal/update"
)

// The releases of the host update tests: the host's and the coordinator's.
const (
	hostOldRelease = "v0.5.0"
	hostNewRelease = "v0.6.0"
)

// The host fixture of the host update tests.
const (
	hostFixtureRules         = "fixture rules"
	hostFixtureClaude        = "/usr/local/bin/claude"
	hostFixtureClaudeVersion = "2.1.290"
	hostFixtureWorkspaces    = "/srv/workspaces"
	hostFixtureSource        = "/srv/source"
	hostFixtureDraining      = "host-c"
	hostDrainsWord           = "drains"
	hostFixtureCommand       = "hosts"
	hostFixtureCoordinator   = "coordinator.example"
	hostFixtureWorker        = "remote-worker"
)

// withCoordinatorVersion makes the coordinator run version.
func withCoordinatorVersion(t *testing.T, version string) {
	t.Helper()
	prev := coordinatorVersion
	coordinatorVersion = func() string { return version }
	t.Cleanup(func() { coordinatorVersion = prev })
}

// hostUpdateServer is a prepared execution host runtime on release version.
func hostUpdateServer(t *testing.T, a *app, version string) *execution.Server {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, err := execution.SigningKey(filepath.Join(root, "key"))
	if err != nil {
		t.Fatal(err)
	}
	endpoint := execution.Endpoint{SSH: hostFixtureCoordinator, Executable: sshTestExecutable, Config: sshTestConfig}
	s := &execution.Server{Root: filepath.Join(root, "execution"), Rules: []byte(hostFixtureRules), Version: version, Executable: sshTestExecutable, ConfigPath: "/etc/beekeeper/host.yaml", Config: execution.HostConfig{Identity: sshTestSelector, RulesFile: filepath.Join(root, "host-rules.md"), WorkspaceRoot: filepath.Join(root, "workspaces"), Repositories: map[string]string{sshTestRepository: filepath.Join(root, "source")}, Harnesses: map[string]execution.HarnessConfig{claudeComm: {Executable: hostFixtureClaude, Version: hostFixtureClaudeVersion}}, Coordinators: map[string]execution.Coordinator{sshTestCoordinator: {PublicKey: execution.PublicKey(key), Callback: endpoint}}, MaxBusy: 1}}
	if _, err := s.Prepare(s.Policy()); err != nil {
		t.Fatal(err)
	}
	a.executionRuntime = func() (*execution.Server, error) { return s, nil }
	return s
}

// hostInstall replaces the host's install and the installed
// release's prepare: install records its calls, prepare answers answer.
type hostInstall struct {
	// installs is each release installed by the host's own lookup, given
	// each release installed as the coordinator's lookup gave it.
	installs, given, restarts []string
	prepareArgs               []string
	answer                    execution.HostUpdate
}

func stubHostInstall(t *testing.T, h *hostInstall) {
	t.Helper()
	install, installGiven, prepare, restart := installVersion, installRelease, runHostPrepare, restartOnInstalled
	t.Cleanup(func() {
		installVersion, installRelease, runHostPrepare, restartOnInstalled = install, installGiven, prepare, restart
	})
	installRelease = func(_ context.Context, _ io.Writer, rel update.Release) (update.Result, error) {
		h.given = append(h.given, rel.Tag)
		return update.Result{Latest: rel.Tag, Path: sshTestExecutable, Updated: true}, nil
	}
	restartOnInstalled = func(_ context.Context, _ *app, path string, _ time.Time) {
		h.restarts = append(h.restarts, path)
	}
	installVersion = func(_ context.Context, _ io.Writer, version string) (update.Result, error) {
		h.installs = append(h.installs, version)
		return update.Result{Latest: version, Path: sshTestExecutable, Updated: true}, nil
	}
	runHostPrepare = func(_ context.Context, path string, args ...string) ([]byte, error) {
		h.prepareArgs = append([]string{path}, args...)
		return json.Marshal(h.answer)
	}
}

func TestHostSelfUpdatePreparesTheRunningRelease(t *testing.T) {
	a, _ := executionTestApp(t)
	s := hostUpdateServer(t, a, hostNewRelease)
	h := &hostInstall{}
	stubHostInstall(t, h)
	got := a.hostSelfUpdate(context.Background(), execution.Protocol, hostNewRelease, nil)
	if got.Error != "" || got.Version != hostNewRelease || got.Identity != sshTestSelector || got.PolicyDigest != s.Policy().Digest() {
		t.Fatalf("self-update answered %+v, want the prepared policy %s", got, s.Policy().Digest())
	}
	if len(h.installs) != 0 {
		t.Errorf("the running release was installed again: %v", h.installs)
	}
	if _, err := s.InstalledPolicy(); err != nil {
		t.Errorf("installed policy: %v", err)
	}
}

func TestHostSelfUpdateInstallsTheReleaseWhichPreparesItsOwnPolicy(t *testing.T) {
	a, _ := executionTestApp(t)
	s := hostUpdateServer(t, a, hostOldRelease)
	want := execution.HostUpdate{Protocol: execution.Protocol, Identity: sshTestSelector, Version: hostNewRelease, PolicyDigest: execution.Digest([]byte("new release policy"))}
	h := &hostInstall{answer: want}
	stubHostInstall(t, h)
	got := a.hostSelfUpdate(context.Background(), execution.Protocol, hostNewRelease, nil)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("self-update answered %+v, want the installed release's %+v", got, want)
	}
	if !slices.Equal(h.installs, []string{hostNewRelease}) || !slices.Equal(h.restarts, []string{sshTestExecutable}) {
		t.Errorf("installs %v, restarts %v", h.installs, h.restarts)
	}
	wantArgs := []string{sshTestExecutable, "--" + configFlag, s.ConfigPath, agentsName, hostFixtureCommand, "self-update", "--protocol", "1", "--" + hostVersionFlag, hostNewRelease}
	if !slices.Equal(h.prepareArgs, wantArgs) {
		t.Errorf("the installed release ran %v, want %v", h.prepareArgs, wantArgs)
	}
}

func TestHostSelfUpdateHoldsAReleaseThatDoesNotPrepare(t *testing.T) {
	for name, answer := range map[string]execution.HostUpdate{
		"the release refuses":   {Protocol: execution.Protocol, Identity: sshTestSelector, Version: hostNewRelease, Error: "policy bundle differs"},
		"the release no digest": {Protocol: execution.Protocol, Identity: sshTestSelector, Version: hostNewRelease},
		"another release":       {Protocol: execution.Protocol, Identity: sshTestSelector, Version: hostOldRelease, PolicyDigest: execution.Digest([]byte("x"))},
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := executionTestApp(t)
			s := hostUpdateServer(t, a, hostOldRelease)
			// The failed release left files of its own behind.
			if err := os.WriteFile(filepath.Join(s.Root, "CLAUDE.md"), []byte("the new release's rules"), 0o600); err != nil {
				t.Fatal(err)
			}
			h := &hostInstall{answer: answer}
			stubHostInstall(t, h)
			got := a.hostSelfUpdate(context.Background(), execution.Protocol, hostNewRelease, nil)
			if !strings.Contains(got.Error, "did not prepare its policy") || got.PolicyDigest != "" {
				t.Fatalf("self-update answered %+v, want a prepare failure", got)
			}
			if len(h.restarts) != 0 {
				t.Errorf("restarts onto an unprepared release %v", h.restarts)
			}
			if got, err := os.ReadFile(filepath.Join(s.Root, "CLAUDE.md")); err != nil || string(got) != "the new release's rules" {
				t.Errorf("failed prepare restored old policy: %q, %v", got, err)
			}
		})
	}
}

func TestHostSelfUpdateChangesNothingWhileExecutionsRun(t *testing.T) {
	a, r := executionTestApp(t)
	s := hostUpdateServer(t, a, hostOldRelease)
	j := execution.Journal{Phase: execution.Running, Request: r, Digest: r.Digest(), Handle: execution.Handle{Ref: r.Ref, Unit: execution.UnitName(r.Ref), ConversationID: r.ConversationID}}
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, string(r.Ref.Execution)+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	h := &hostInstall{}
	stubHostInstall(t, h)
	got := a.hostSelfUpdate(context.Background(), execution.Protocol, hostNewRelease, nil)
	if !slices.Equal(got.Executions, []execution.Ref{r.Ref}) || got.Busy != 1 || got.Error != "" || got.PolicyDigest != "" || got.Version != hostOldRelease {
		t.Fatalf("self-update answered %+v, want busy and unchanged", got)
	}
	if len(h.installs) != 0 {
		t.Errorf("installed %v while an execution runs", h.installs)
	}
}

// An execution between turns, a parked worker's waiting for its wake, does
// not hold the host's update back.
func TestHostSelfUpdateGoesAheadUnderAnExecutionBetweenTurns(t *testing.T) {
	a, r := executionTestApp(t)
	s := hostUpdateServer(t, a, hostOldRelease)
	j := execution.Journal{Phase: execution.Running, Ready: true, Request: r, Digest: r.Digest(), Handle: execution.Handle{Ref: r.Ref, Unit: execution.UnitName(r.Ref), ConversationID: r.ConversationID}}
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, string(r.Ref.Execution)+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := execution.Digest([]byte("prepared by the new release"))
	h := &hostInstall{answer: execution.HostUpdate{Protocol: execution.Protocol, Identity: sshTestSelector, Version: hostNewRelease, PolicyDigest: digest}}
	stubHostInstall(t, h)
	got := a.hostSelfUpdate(context.Background(), execution.Protocol, hostNewRelease, nil)
	if got.Busy != 0 || got.Error != "" || got.PolicyDigest != digest || got.Version != hostNewRelease {
		t.Fatalf("self-update answered %+v, want the new release", got)
	}
	if !slices.Equal(h.installs, []string{hostNewRelease}) {
		t.Errorf("installed %v, want %s", h.installs, hostNewRelease)
	}
}

func TestHostSelfUpdateRefusesAWorkerOrAnotherProtocol(t *testing.T) {
	a, r := executionTestApp(t)
	hostUpdateServer(t, a, hostOldRelease)
	h := &hostInstall{}
	stubHostInstall(t, h)
	for name, run := range map[string]func() execution.HostUpdate{
		"another protocol": func() execution.HostUpdate {
			return a.hostSelfUpdate(context.Background(), execution.Protocol+1, hostNewRelease, nil)
		},
		"no release": func() execution.HostUpdate {
			return a.hostSelfUpdate(context.Background(), execution.Protocol, "latest", nil)
		},
		"a managed worker": func() execution.HostUpdate {
			t.Setenv(execution.EnvID, string(r.Ref.Execution))
			return a.hostSelfUpdate(context.Background(), execution.Protocol, hostNewRelease, nil)
		},
	} {
		if got := run(); got.Error == "" {
			t.Errorf("%s: self-update answered %+v", name, got)
		}
	}
	if len(h.installs) != 0 {
		t.Errorf("installed %v", h.installs)
	}
}

// stubHostUpdates answers the coordinator's release lookup and its SSH
// update of each host, which must be handed the lookup.
func stubHostUpdates(t *testing.T, a *app, answers map[execution.HostID]func(string) (execution.HostUpdate, error)) *[]string {
	t.Helper()
	lookup := lookupRelease
	t.Cleanup(func() { lookupRelease = lookup })
	lookupRelease = func(_ context.Context, version string) (update.Release, error) {
		return update.Release{Tag: version, Assets: []update.Asset{{ID: 1, Name: "beekeeper-linux-amd64", URL: "https://github.com/" + update.Repository + "/releases/download/" + version + "/beekeeper-linux-amd64"}}}, nil
	}
	var calls []string
	a.executionUpdate = func(_ context.Context, h execution.Host, version string, given []byte) (execution.HostUpdate, error) {
		calls = append(calls, string(h.Identity)+" "+version)
		var rel update.Release
		if err := execution.Decode(given, &rel); err != nil || rel.Tag != version || len(rel.Assets) != 1 {
			t.Errorf("the host was handed %q (%v), want the coordinator's lookup of %s", given, err, version)
		}
		return answers[h.Identity](version)
	}
	return &calls
}

func pinnedAnswer(id execution.HostID, digest string) func(string) (execution.HostUpdate, error) {
	return func(version string) (execution.HostUpdate, error) {
		return execution.HostUpdate{Protocol: execution.Protocol, Identity: id, Version: version, PolicyDigest: digest}, nil
	}
}

func TestHostsUpdatePinsTheDigestInStateWithoutAConfiguredOne(t *testing.T) {
	a, _ := sshStartTestApp(t)
	withCoordinatorVersion(t, hostNewRelease)
	h := a.cfg.Agents.Hosts[sshTestSelector]
	h.PolicyDigest = ""
	a.cfg.Agents.Hosts[sshTestSelector] = h
	if _, err := a.executionHost(sshTestSelector); Code(err) != ExitRefused {
		t.Fatalf("an unpinned host without a configured digest: %v", err)
	}
	digest := execution.Digest([]byte("prepared on the host"))
	stubHostUpdates(t, a, map[execution.HostID]func(string) (execution.HostUpdate, error){sshTestSelector: pinnedAnswer(sshTestSelector, digest)})
	var probed []string
	probe := a.executionProbe
	a.executionProbe = func(ctx context.Context, h execution.Host) (execution.Capabilities, error) {
		probed = append(probed, h.PolicyDigest)
		return probe(ctx, h)
	}
	out, err := runAgents(a, hostFixtureCommand, hostUpdateCmdName, sshTestSelector)
	if err != nil || !strings.Contains(out, hostNewRelease) || !strings.Contains(out, digest) {
		t.Fatalf("hosts update: %s, %v", out, err)
	}
	if !slices.Equal(probed, []string{digest}) {
		t.Errorf("checked with %v, want the prepared digest", probed)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	pin := st.HostPin(sshTestSelector)
	if pin == nil || pin.Version != hostNewRelease || pin.PolicyDigest != digest || pin.Identity != sshTestSelector || pin.Held != "" {
		t.Fatalf("pin %+v", pin)
	}
	got, err := a.executionHost(sshTestSelector)
	if err != nil || got.PolicyDigest != digest {
		t.Fatalf("pinned host %+v, %v", got, err)
	}
	if err := a.admitExecution(sshTestSelector, got); err != nil {
		t.Errorf("the pinned host takes no start: %v", err)
	}
	// A newer coordinator does not revoke the already pinned host policy.
	withCoordinatorVersion(t, "v0.7.0")
	if err := a.admitExecution(sshTestSelector, got); err != nil {
		t.Errorf("a host behind the coordinator: %v", err)
	}
}

func TestHostsUpdateHoldsAFailingHostWhileTheOthersRun(t *testing.T) {
	a, _ := sshStartTestApp(t)
	withCoordinatorVersion(t, hostNewRelease)
	digest := execution.Digest([]byte("prepared on host-b"))
	failing := true
	stubHostUpdates(t, a, map[execution.HostID]func(string) (execution.HostUpdate, error){
		sshTestSelector: func(version string) (execution.HostUpdate, error) {
			if failing {
				return execution.HostUpdate{}, errors.New("no signed release for this platform")
			}
			return pinnedAnswer(sshTestSelector, digest)(version)
		},
		sshTestOtherSelector: pinnedAnswer(sshTestOtherSelector, digest),
	})
	if _, err := runAgents(a, hostFixtureCommand, hostUpdateCmdName, sshTestSelector); Code(err) != ExitRefused || !strings.Contains(err.Error(), "held") {
		t.Fatalf("a failing update: %v", err)
	}
	if _, err := runAgents(a, hostFixtureCommand, hostUpdateCmdName, sshTestOtherSelector); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	pin := st.HostPin(sshTestSelector)
	if pin == nil || !strings.Contains(pin.Held, "no signed release") || pin.HeldVersion != hostNewRelease || pin.HeldAt.IsZero() {
		t.Fatalf("held pin %+v", pin)
	}
	held, _ := a.executionHost(sshTestSelector)
	if err := a.admitExecution(sshTestSelector, held); Code(err) != ExitRefused || !strings.Contains(err.Error(), "held") {
		t.Errorf("a held host: %v", err)
	}
	other, _ := a.executionHost(sshTestOtherSelector)
	if err := a.admitExecution(sshTestOtherSelector, other); err != nil {
		t.Errorf("the other host stopped with the held one: %v", err)
	}
	// A start on the held host is refused before anything is reserved.
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte(sshTestTask), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runAgents(a, agentStartName, "--name", hostFixtureWorker, brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--model", bgModel); Code(err) != ExitRefused {
		t.Errorf("a start on a held host: %v", err)
	}
	if st, _ := a.store.Read(); len(st.Starts) != 0 {
		t.Errorf("a start on a held host reserved %d", len(st.Starts))
	}
	failing = false
	if _, err := runAgents(a, hostFixtureCommand, hostUpdateCmdName, sshTestSelector); err != nil {
		t.Fatal(err)
	}
	if st, _ := a.store.Read(); st.HostPin(sshTestSelector).Held != "" {
		t.Errorf("a successful update left the hold: %+v", st.HostPin(sshTestSelector))
	}
}

func TestHostsUpdateDrainsAHostWithExecutions(t *testing.T) {
	a, _ := sshStartTestApp(t)
	withCoordinatorVersion(t, hostNewRelease)
	calls := stubHostUpdates(t, a, map[execution.HostID]func(string) (execution.HostUpdate, error){
		sshTestSelector: func(string) (execution.HostUpdate, error) {
			return execution.HostUpdate{Protocol: execution.Protocol, Identity: sshTestSelector, Version: hostOldRelease, Busy: 2}, nil
		},
	})
	if _, err := runAgents(a, hostFixtureCommand, hostUpdateCmdName, sshTestSelector); Code(err) != ExitRefused || !strings.Contains(err.Error(), hostDrainsWord) {
		t.Fatalf("a busy host: %v", err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if pin := st.HostPin(sshTestSelector); pin == nil || pin.Held != "" || pin.Version != "" {
		t.Fatalf("a draining host's pin %+v, want neither held nor pinned", pin)
	}
	// Executions the coordinator recorded drain without an SSH call.
	ref := execution.Ref{Host: sshTestSelector, Execution: execution.ExecutionID(uuid.NewString()), Generation: 1}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{Party: state.Party{Name: hostFixtureWorker}, Execution: &state.ExecutionRecord{Selector: sshTestSelector, Handle: execution.Handle{Ref: ref}, Phase: execution.Running, Observation: execution.Observation{ProcessState: execution.Running}}})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	*calls = nil
	if _, err := runAgents(a, hostFixtureCommand, hostUpdateCmdName, sshTestSelector); Code(err) != ExitRefused || len(*calls) != 0 {
		t.Fatalf("recorded executions: %v, calls %v", err, *calls)
	}
}

// A parked worker's execution between turns does not hold its host back:
// the host updates under it, and the coordinator rebinds it to the policy it
// pins. An execution in a turn, or one between turns of a worker that is not
// parked, still drains the host.
func TestHostsUpdateGoesAheadUnderAParkedWorkerBetweenTurns(t *testing.T) {
	a, _ := sshStartTestApp(t)
	withCoordinatorVersion(t, hostNewRelease)
	old := a.cfg.Agents.Hosts[sshTestSelector].PolicyDigest
	digest := execution.Digest([]byte("prepared under a parked worker"))
	calls := stubHostUpdates(t, a, map[execution.HostID]func(string) (execution.HostUpdate, error){sshTestSelector: pinnedAnswer(sshTestSelector, digest)})
	ref := execution.Ref{Host: sshTestSelector, Execution: execution.ExecutionID(uuid.NewString()), Generation: 1}
	handle := execution.Handle{Ref: ref, Unit: execution.UnitName(ref)}
	between := execution.Observation{Handle: handle, ProcessState: execution.Running, Ready: true, PolicyDigest: old, HostPolicyDigest: old}
	record := func(o execution.Observation, park *state.Park) {
		t.Helper()
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			st.Starts = []state.Start{{Party: state.Party{Name: hostFixtureWorker}, Execution: &state.ExecutionRecord{Selector: sshTestSelector, Handle: handle, Phase: execution.Running, Observation: o}}}
			st.Agents = []state.Agent{{Party: state.Party{Name: hostFixtureWorker}, Execution: &ref, Task: sshTestTask, Park: park}}
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	inTurn := between
	inTurn.InTurn = true
	queued := between
	queued.Messages = []execution.Delivery{{Envelope: execution.Envelope{ID: uuid.NewString(), Sequence: 1}, Stage: execution.Stored}}
	for name, c := range map[string]struct {
		o    execution.Observation
		park *state.Park
	}{
		"in a turn":              {inTurn, &state.Park{}},
		"a wake waits":           {queued, &state.Park{}},
		"between turns, no park": {between, nil},
	} {
		record(c.o, c.park)
		*calls = nil
		if _, err := runAgents(a, hostFixtureCommand, hostUpdateCmdName, sshTestSelector); Code(err) != ExitRefused || !strings.Contains(err.Error(), hostDrainsWord) || len(*calls) != 0 {
			t.Fatalf("%s: %v, calls %v", name, err, *calls)
		}
	}
	record(between, &state.Park{})
	*calls = nil
	var rebinds []execution.PolicyRebind
	fake := &testExecutionTransport{}
	fake.inspect = func(_ context.Context, r execution.Ref) (execution.Observation, error) {
		o := between
		o.HostPolicyDigest = digest
		return o, nil
	}
	fake.rebind = func(_ context.Context, r execution.Ref, q execution.PolicyRebind) (execution.Observation, error) {
		rebinds = append(rebinds, q)
		o := between
		o.PolicyDigest, o.HostPolicyDigest = q.To, digest
		return o, nil
	}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	out, err := runAgents(a, hostFixtureCommand, hostUpdateCmdName, sshTestSelector)
	if err != nil || len(*calls) != 1 || !strings.Contains(out, "1 executions between turns rebound") {
		t.Fatalf("a parked worker between turns: %s, %v, calls %v", out, err, *calls)
	}
	if len(rebinds) != 1 || rebinds[0] != (execution.PolicyRebind{From: old, To: digest}) {
		t.Fatalf("rebinds %+v, want %s -> %s", rebinds, old, digest)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if pin := st.HostPin(sshTestSelector); pin == nil || pin.Version != hostNewRelease || pin.PolicyDigest != digest {
		t.Fatalf("pin %+v", pin)
	}
	if o := st.Starts[0].Execution.Observation; o.PolicyDigest != digest {
		t.Errorf("the rebound execution's observation %+v", o)
	}
}

// A host that does not follow the coordinator for more than one release is
// handed on once, with why; one release behind is not, and an update that
// catches up clears it.
func TestSupervisorLoopReportsAHostBehindForMoreThanOneRelease(t *testing.T) {
	r := newLoopRig(t)
	withCoordinatorVersion(t, hostNewRelease)
	r.a.cfg.Agents.Hosts = map[string]config.ExecutionHost{hostFixtureDraining: {Endpoint: execution.Endpoint{SSH: hostFixtureDraining + ".example", Executable: sshTestExecutable, Config: sshTestConfig}, Identity: execution.HostID(hostFixtureDraining), WorkspaceRoot: hostFixtureWorkspaces, Repositories: map[string]string{sshTestRepository: hostFixtureSource}, Harnesses: []string{claudeComm}, MaxBusy: 1}}
	ref := execution.Ref{Host: hostFixtureDraining, Execution: execution.ExecutionID(uuid.NewString()), Generation: 1}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{Party: state.Party{Name: hostFixtureWorker}, Execution: &state.ExecutionRecord{Selector: hostFixtureDraining, Handle: execution.Handle{Ref: ref}, Phase: execution.Running, Observation: execution.Observation{ProcessState: execution.Running, Ready: true, InTurn: true}}})
		st.HostPins = append(st.HostPins, state.HostPin{Selector: hostFixtureDraining, Identity: execution.HostID(hostFixtureDraining), Version: hostOldRelease})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	l := &loop.Loop{Host: r.a, Said: map[string]bool{}}
	pass := func() {
		t.Helper()
		st, err := r.a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		l.UpdateHosts(context.Background(), st)
	}
	behind := func() []state.Event {
		t.Helper()
		findings, err := r.a.store.Events(0, func(e state.Event) bool {
			return e.Verb == "supervisor."+superloop.KindFile && strings.Contains(e.Detail, "host-behind")
		})
		if err != nil {
			t.Fatal(err)
		}
		return findings
	}
	pass()
	pass()
	if n := len(behind()); n != 0 {
		t.Fatalf("%d findings for a host one release behind", n)
	}
	withCoordinatorVersion(t, "v0.7.0")
	pass()
	pass()
	findings := behind()
	if len(findings) != 1 || !strings.Contains(findings[0].Detail, hostFixtureWorker) || !strings.Contains(findings[0].Detail, hostNewRelease) || !strings.Contains(findings[0].Detail, "v0.7.0") || !strings.Contains(findings[0].Detail, "in a turn") {
		t.Fatalf("behind findings %+v", findings)
	}
	if strings.Count(r.out.String(), "SUPERVISOR HOST BEHIND") != 1 {
		t.Fatalf("loop said:\n%s", r.out.String())
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if pin := st.HostPin(hostFixtureDraining); pin.Behind != hostNewRelease || !pin.BehindReported {
		t.Fatalf("pin %+v", pin)
	}
}

func TestHostsUpdateRefusesADevelopmentBuildWhichGatesNoStart(t *testing.T) {
	a, _ := sshStartTestApp(t)
	withCoordinatorVersion(t, "dev")
	calls := stubHostUpdates(t, a, nil)
	if _, err := runAgents(a, hostFixtureCommand, hostUpdateCmdName, sshTestSelector); Code(err) != ExitRefused || len(*calls) != 0 {
		t.Fatalf("a development build updated a host: %v, %v", err, *calls)
	}
	h, err := a.executionHost(sshTestSelector)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.admitExecution(sshTestSelector, h); err != nil {
		t.Errorf("a development build gated the configured digest's host: %v", err)
	}
}

func TestSupervisorLoopUpdatesHostsAndHandsOnEachHoldOnce(t *testing.T) {
	r := newLoopRig(t)
	withCoordinatorVersion(t, hostNewRelease)
	r.a.cfg.Agents.Hosts = map[string]config.ExecutionHost{}
	for _, selector := range []string{sshTestSelector, sshTestOtherSelector, hostFixtureDraining} {
		r.a.cfg.Agents.Hosts[selector] = execution.Host{Endpoint: execution.Endpoint{SSH: selector + ".example", Executable: sshTestExecutable, Config: sshTestConfig}, Identity: execution.HostID(selector), WorkspaceRoot: hostFixtureWorkspaces, Repositories: map[string]string{sshTestRepository: hostFixtureSource}, Harnesses: []string{claudeComm}, MaxBusy: 1}
	}
	ref := execution.Ref{Host: hostFixtureDraining, Execution: execution.ExecutionID(uuid.NewString()), Generation: 1}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{Party: state.Party{Name: hostFixtureWorker}, Execution: &state.ExecutionRecord{Selector: hostFixtureDraining, Handle: execution.Handle{Ref: ref}, Phase: execution.Running}})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	// The command records what agents hosts update records.
	r.answer = func(args []string) (string, int) {
		if len(args) != 4 || args[0] != agentsName || args[1] != hostFixtureCommand || args[2] != hostUpdateCmdName {
			return "", 0
		}
		selector := args[3]
		err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
			p := state.HostPin{Selector: selector, Identity: execution.HostID(selector), Tried: r.a.now}
			if selector == sshTestSelector {
				p.Held, p.HeldAt, p.HeldVersion = "unreachable", r.a.now, hostNewRelease
			} else {
				p.Version, p.PolicyDigest = hostNewRelease, execution.Digest([]byte(selector))
			}
			st.HostPins = slices.DeleteFunc(st.HostPins, func(x state.HostPin) bool { return x.Selector == selector })
			st.HostPins = append(st.HostPins, p)
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if selector == sshTestSelector {
			return "execution host " + selector + " is held", ExitRefused
		}
		return selector + " runs " + hostNewRelease, 0
	}
	l := &loop.Loop{Host: r.a, Said: map[string]bool{}}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	l.UpdateHosts(context.Background(), st)
	var updated []string
	for _, c := range r.calls {
		if len(c) == 4 && c[2] == hostUpdateCmdName {
			updated = append(updated, c[3])
		}
	}
	if !slices.Equal(updated, []string{sshTestSelector, sshTestOtherSelector}) {
		t.Fatalf("updated %v, want host-a and host-b, host-c draining", updated)
	}
	out := r.out.String()
	if strings.Count(out, "SUPERVISOR HOST HELD") != 1 || !strings.Contains(out, "SUPERVISOR HOST UPDATED "+sshTestOtherSelector) || !strings.Contains(out, "SUPERVISOR HOST DRAINS "+hostFixtureDraining) {
		t.Fatalf("loop said:\n%s", out)
	}
	findings, err := r.a.store.Events(0, func(e state.Event) bool { return e.Verb == "supervisor."+superloop.KindFile })
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || !strings.Contains(findings[0].Detail, loop.FileReportSeparator) || !strings.Contains(findings[0].Detail, "unreachable") {
		t.Fatalf("host hold findings %+v", findings)
	}
	// A later pass within the retry interval updates nothing again and
	// hands the hold on no second time.
	r.calls = nil
	r.a.now = r.a.now.Add(time.Minute)
	st, _ = r.a.store.Read()
	l.UpdateHosts(context.Background(), st)
	for _, c := range r.calls {
		if len(c) == 4 && c[2] == hostUpdateCmdName {
			t.Errorf("updated %v again", c)
		}
	}
	if findings, _ := r.a.store.Events(0, func(e state.Event) bool { return e.Verb == "supervisor."+superloop.KindFile }); len(findings) != 1 {
		t.Errorf("%d findings, want the one", len(findings))
	}
	// An hour on, the held host is tried again.
	r.calls = nil
	r.a.now = r.a.now.Add(loop.HostUpdateRetry)
	st, _ = r.a.store.Read()
	l.UpdateHosts(context.Background(), st)
	if len(r.calls) == 0 || r.calls[0][3] != sshTestSelector {
		t.Errorf("the held host was not tried again: %v", r.calls)
	}
}

func TestReleaseVersionIsATagNotABuild(t *testing.T) {
	for v, want := range map[string]bool{
		"v0.174.0":       true,
		"v0.174.0-rc.22": true,
		"v1.0.0":         true,
		"dev":            false,
		"0.174.0":        false,
		"v0.174":         false,
		"v0.173.3-r41451705t20261102031105hdf6b6d4": false,
		"v0.3.1-0.20260924210000-edd882d0a1b2":      false,
		"v0.174.0+dirty":                            false,
		"v0.174.0-rc.1; true":                       false,
	} {
		if got := releaseVersion(v); got != want {
			t.Errorf("releaseVersion(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestHostSelfUpdateInstallsTheCoordinatorsLookup(t *testing.T) {
	a, _ := executionTestApp(t)
	hostUpdateServer(t, a, hostOldRelease)
	want := execution.HostUpdate{Protocol: execution.Protocol, Identity: sshTestSelector, Version: hostNewRelease, PolicyDigest: execution.Digest([]byte("new release policy"))}
	h := &hostInstall{answer: want}
	stubHostInstall(t, h)
	// A field a newer coordinator sends is no reason to refuse.
	given := []byte(`{"tag":"` + hostNewRelease + `","later":true}`)
	if got := a.hostSelfUpdate(context.Background(), execution.Protocol, hostNewRelease, given); !reflect.DeepEqual(got, want) {
		t.Fatalf("self-update answered %+v, want %+v", got, want)
	}
	if !slices.Equal(h.given, []string{hostNewRelease}) || len(h.installs) != 0 {
		t.Errorf("installed the coordinator's lookup %v and its own %v, want only the coordinator's", h.given, h.installs)
	}
}

func TestHostSelfUpdateRefusesALookupOfAnotherRelease(t *testing.T) {
	a, _ := executionTestApp(t)
	hostUpdateServer(t, a, hostOldRelease)
	h := &hostInstall{}
	stubHostInstall(t, h)
	for name, given := range map[string]string{
		"another release": `{"tag":"` + hostOldRelease + `"}`,
		"a broken record": `{"tag":`,
		"no tag":          `{"assets":[]}`,
	} {
		if got := a.hostSelfUpdate(context.Background(), execution.Protocol, hostNewRelease, []byte(given)); got.Error == "" {
			t.Errorf("%s: self-update answered %+v", name, got)
		}
	}
	if len(h.given) != 0 || len(h.installs) != 0 {
		t.Errorf("installed %v %v", h.given, h.installs)
	}
}

func TestHostsUpdateRetainsAHostWhoseReleaseLookupFails(t *testing.T) {
	a, _ := sshStartTestApp(t)
	withCoordinatorVersion(t, hostNewRelease)
	calls := stubHostUpdates(t, a, nil)
	lookupRelease = func(context.Context, string) (update.Release, error) {
		return update.Release{}, errors.New("GitHub rate limit exceeded; resets at 2026-11-03T15:00:00Z")
	}
	if _, err := runAgents(a, hostFixtureCommand, hostUpdateCmdName, sshTestSelector); Code(err) != ExitRefused || !strings.Contains(err.Error(), "rate limit exceeded; resets at") {
		t.Fatalf("a refused lookup: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("the host was asked to update without the coordinator's lookup: %v", *calls)
	}
	if st, _ := a.store.Read(); st.HostPin(sshTestSelector) == nil || st.HostPin(sshTestSelector).Held != "" || st.HostPin(sshTestSelector).UpdateError == "" {
		t.Errorf("a refused lookup must defer only the update")
	}
	h, err := a.executionHost(sshTestSelector)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.admitExecution(sshTestSelector, h); err != nil {
		t.Fatalf("lookup failure blocked starts: %v", err)
	}
}

func TestHostReleaseInputReadsWhatTheCoordinatorSends(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = null.Close() })
	for name, tc := range map[string]struct {
		in   io.Reader
		want string
		ok   bool
	}{
		litADevice:    {null, "", true},
		"no input":    {strings.NewReader(""), "", true},
		"a lookup":    {strings.NewReader(" {\"tag\":\"v0.6.0\"}\n"), `{"tag":"v0.6.0"}`, true},
		"over limits": {strings.NewReader(strings.Repeat(" ", execution.MaxWire+1)), "", false},
	} {
		got, err := hostReleaseInput(tc.in)
		if (err == nil) != tc.ok || string(got) != tc.want {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
}

func TestHostSelfUpdateFollowsCoordinatorBackToRecoveryRelease(t *testing.T) {
	a, _ := executionTestApp(t)
	hostUpdateServer(t, a, hostNewRelease)
	want := execution.HostUpdate{Protocol: execution.Protocol, Identity: sshTestSelector, Version: hostOldRelease, PolicyDigest: execution.Digest([]byte("recovery release policy"))}
	h := &hostInstall{answer: want}
	stubHostInstall(t, h)
	install := installVersion
	installVersion = func(ctx context.Context, w io.Writer, version string) (update.Result, error) {
		if update.OptionsFrom(ctx).HostRollback == nil {
			t.Fatal("coordinator downgrade did not supply the state compatibility guard")
		}
		return install(ctx, w, version)
	}
	got := a.hostSelfUpdate(context.Background(), execution.Protocol, hostOldRelease, nil)
	if !reflect.DeepEqual(got, want) || !slices.Equal(h.installs, []string{hostOldRelease}) || len(h.restarts) != 1 {
		t.Fatalf("coordinator downgrade: %+v, installs %v, restarts %v", got, h.installs, h.restarts)
	}
}

func TestSupervisorLoopRetriesRateLimitedHostUpdateAfterResetAndCachesRelease(t *testing.T) {
	a, _ := sshStartTestApp(t)
	withCoordinatorVersion(t, hostNewRelease)
	digest := execution.Digest([]byte("prepared after rate limit reset"))
	stubHostUpdates(t, a, map[execution.HostID]func(string) (execution.HostUpdate, error){
		sshTestSelector:      pinnedAnswer(sshTestSelector, digest),
		sshTestOtherSelector: pinnedAnswer(sshTestOtherSelector, digest),
	})
	lookups := 0
	prevLookup := lookupRelease
	t.Cleanup(func() { lookupRelease = prevLookup })
	lookupRelease = func(ctx context.Context, version string) (update.Release, error) {
		lookups++
		return prevLookup(ctx, version)
	}
	if _, err := runAgents(a, hostFixtureCommand, hostUpdateCmdName, sshTestSelector); err != nil {
		t.Fatal(err)
	}
	if _, err := runAgents(a, hostFixtureCommand, hostUpdateCmdName, sshTestOtherSelector); err != nil {
		t.Fatal(err)
	}
	if lookups != 1 {
		t.Fatalf("lookupRelease called %d times across 2 hosts for the same release, want 1", lookups)
	}

	r := newLoopRig(t)
	withCoordinatorVersion(t, hostNewRelease)
	r.a.cfg.Agents.Hosts = map[string]config.ExecutionHost{
		sshTestSelector: {Endpoint: execution.Endpoint{SSH: sshTestSelector + ".example", Executable: sshTestExecutable, Config: sshTestConfig}, Identity: sshTestSelector, WorkspaceRoot: hostFixtureWorkspaces, Repositories: map[string]string{sshTestRepository: hostFixtureSource}, Harnesses: []string{claudeComm}, MaxBusy: 1},
	}
	resetAt := r.a.now.Add(5 * time.Minute).UTC().Truncate(time.Second)
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.HostPins = []state.HostPin{{
			Selector:    sshTestSelector,
			Identity:    sshTestSelector,
			Tried:       r.a.now,
			HeldAt:      r.a.now,
			HeldVersion: hostNewRelease,
			Held:        "looking up release " + hostNewRelease + " of giantswarm/beekeeper: GitHub rate limit exceeded; resets at " + resetAt.Format(time.RFC3339),
		}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	l := &loop.Loop{Host: r.a, Said: map[string]bool{}}
	r.a.now = resetAt.Add(-time.Minute)
	st, _ := r.a.store.Read()
	l.UpdateHosts(context.Background(), st)
	if len(r.calls) != 0 {
		t.Fatalf("retried rate-limited host before reset: %v", r.calls)
	}
	r.a.now = resetAt.Add(time.Second)
	st, _ = r.a.store.Read()
	l.UpdateHosts(context.Background(), st)
	if len(r.calls) != 1 || !slices.Equal(r.calls[0], []string{agentsName, hostFixtureCommand, hostUpdateCmdName, sshTestSelector}) {
		t.Fatalf("rate-limited host did not retry immediately after reset: %v", r.calls)
	}
}

func TestHostsUpdateDefersProbationWithoutHoldingOrDelayingRetry(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		name := coordinatorProbationPassed
		if rollback {
			name = coordinatorProbationRolledBack
		}
		t.Run(name, func(t *testing.T) {
			a, _ := sshStartTestApp(t)
			withCoordinatorVersion(t, hostNewRelease)
			j := &update.Journal{Probation: coordinatorProbationPending, Candidate: update.Binary{Version: hostNewRelease}, Previous: &update.Binary{Version: hostOldRelease}}
			previous := coordinatorJournal
			coordinatorJournal = func() (*update.Journal, error) { return j, nil }
			t.Cleanup(func() { coordinatorJournal = previous })
			digest := execution.Digest([]byte("settled policy"))
			calls := stubHostUpdates(t, a, map[execution.HostID]func(string) (execution.HostUpdate, error){sshTestSelector: pinnedAnswer(sshTestSelector, digest)})
			for range 2 {
				if _, err := a.updateExecutionHost(t.Context(), sshTestSelector); err == nil || !strings.Contains(err.Error(), hostUpdateDeferred) {
					t.Fatalf("probation update: %v", err)
				}
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(*calls) != 0 || st.HostPin(sshTestSelector) != nil {
				t.Fatalf("probation changed host: calls %v, pin %+v", *calls, st.HostPin(sshTestSelector))
			}
			version := hostNewRelease
			j.Probation = coordinatorProbationPassed
			if rollback {
				j.Probation = coordinatorProbationRolledBack
				version = hostOldRelease
				withCoordinatorVersion(t, version)
			}
			if _, err := a.updateExecutionHost(t.Context(), sshTestSelector); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(*calls, []string{sshTestSelector + " " + version}) {
				t.Fatalf("updated releases %v", *calls)
			}
			st, err = a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if pin := st.HostPin(sshTestSelector); pin == nil || pin.Held != "" || pin.Version != version {
				t.Fatalf("settled pin %+v", pin)
			}
		})
	}
}

func TestSupervisorDefersHostsUntilCoordinatorProbationPasses(t *testing.T) {
	r := newLoopRig(t)
	withCoordinatorVersion(t, hostNewRelease)
	r.a.cfg.Agents.Hosts = map[string]config.ExecutionHost{sshTestSelector: {Identity: sshTestSelector}}
	j := &update.Journal{Probation: coordinatorProbationPending, Candidate: update.Binary{Version: hostNewRelease}}
	previous := coordinatorJournal
	coordinatorJournal = func() (*update.Journal, error) { return j, nil }
	t.Cleanup(func() { coordinatorJournal = previous })
	l := &loop.Loop{Host: r.a, Said: map[string]bool{}}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	l.UpdateHosts(t.Context(), st)
	if len(r.calls) != 0 || strings.Contains(r.out.String(), "HOST HELD") {
		t.Fatalf("probation attempted updates: %v, %s", r.calls, r.out.String())
	}
	j.Probation = coordinatorProbationPassed
	l.UpdateHosts(t.Context(), st)
	if len(r.calls) != 1 || !slices.Equal(r.calls[0], []string{agentsName, "hosts", hostUpdateCmdName, sshTestSelector}) {
		t.Fatalf("no immediate retry after probation: %v", r.calls)
	}
}

func TestHostsProbationRefusalDoesNotHoldHost(t *testing.T) {
	a, _ := sshStartTestApp(t)
	withCoordinatorVersion(t, hostNewRelease)
	stubHostUpdates(t, a, map[execution.HostID]func(string) (execution.HostUpdate, error){
		sshTestSelector: func(string) (execution.HostUpdate, error) {
			return execution.HostUpdate{}, fmt.Errorf("install refused: %w", update.ErrProbationWatched)
		},
	})
	if _, err := a.updateExecutionHost(t.Context(), sshTestSelector); err == nil || !strings.Contains(err.Error(), hostUpdateDeferred) {
		t.Fatalf("host probation: %v", err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if pin := st.HostPin(sshTestSelector); pin != nil {
		t.Fatalf("probation created pin or hold: %+v", pin)
	}
	if loop.HostHoldWait(state.HostPin{Held: update.ErrProbationWatched.Error(), Tried: a.now}, nil, a.now) {
		t.Fatal("legacy probation hold delays retry")
	}
}

func TestCoordinatorHostUpdateWaitsForReadableMatchingVerdict(t *testing.T) {
	previous := coordinatorJournal
	t.Cleanup(func() { coordinatorJournal = previous })
	for _, phase := range []string{"prepared", coordinatorProbationPending, "rolling-back", coordinatorProbationPassed, coordinatorProbationRolledBack} {
		j := &update.Journal{Probation: phase, Candidate: update.Binary{Version: hostNewRelease}, Previous: &update.Binary{Version: hostOldRelease}}
		coordinatorJournal = func() (*update.Journal, error) { return j, nil }
		for _, version := range []string{hostOldRelease, hostNewRelease} {
			want := phase == coordinatorProbationPassed && version == hostNewRelease || phase == coordinatorProbationRolledBack && version == hostOldRelease
			if err := coordinatorHostUpdateReady(version); (err == nil) != want {
				t.Fatalf("%s on %s: %v", phase, version, err)
			}
		}
	}
	coordinatorJournal = func() (*update.Journal, error) { return nil, errors.New("unreadable journal") }
	if err := coordinatorHostUpdateReady(hostNewRelease); err == nil {
		t.Fatal("unreadable probation allowed an update")
	}
}

func TestHostsUpdateRaisesGoPinWhenServedRepositoryToolchainAdvances(t *testing.T) {
	const wantedGoVersion = "1.27.4"
	a, _ := sshStartTestApp(t)
	withCoordinatorVersion(t, hostNewRelease)
	repoDir := filepath.Join(a.cfg.Projects.Dir, "beekeeper")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module github.com/giantswarm/beekeeper\n\ngo 1.26.0\n\ntoolchain go"+wantedGoVersion+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := execution.Digest([]byte("prepared with go" + wantedGoVersion))
	lookup := lookupRelease
	t.Cleanup(func() { lookupRelease = lookup })
	lookupRelease = func(_ context.Context, version string) (update.Release, error) {
		return update.Release{Tag: version, Assets: []update.Asset{{ID: 1, Name: "beekeeper-linux-amd64", URL: "https://github.com/" + update.Repository + "/releases/download/" + version + "/beekeeper-linux-amd64"}}}, nil
	}
	var gotTools map[string]string
	a.executionUpdate = func(_ context.Context, h execution.Host, version string, given []byte) (execution.HostUpdate, error) {
		var rel update.Release
		if err := execution.Decode(given, &rel); err != nil {
			t.Fatal(err)
		}
		gotTools = rel.Tools
		return execution.HostUpdate{Protocol: execution.Protocol, Identity: h.Identity, Version: version, PolicyDigest: digest}, nil
	}
	probe := a.executionProbe
	a.executionProbe = func(ctx context.Context, h execution.Host) (execution.Capabilities, error) {
		caps, err := probe(ctx, h)
		caps.Tools = map[string]string{execution.ToolGitHub: "2.83.0", execution.ToolGo: wantedGoVersion}
		return caps, err
	}
	if _, err := runAgents(a, hostFixtureCommand, hostUpdateCmdName, sshTestSelector); err != nil {
		t.Fatal(err)
	}
	if gotTools[execution.ToolGo] != wantedGoVersion {
		t.Fatalf("host update sent tools %v, want go %s", gotTools, wantedGoVersion)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	pin := st.HostPin(sshTestSelector)
	if pin == nil || pin.Tools[execution.ToolGo] != wantedGoVersion {
		t.Fatalf("host pin tools = %+v, want go %s", pin, wantedGoVersion)
	}
	// When a served repository advances to 1.27.5 on the same coordinator
	// release, hostPinToolsCurrent reports the pin stale so updateHosts runs.
	if !a.hostPinToolsCurrent(*pin, a.cfg.Agents.Hosts[sshTestSelector]) {
		t.Fatal("pin at 1.27.4 reported stale for 1.27.4")
	}
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module github.com/giantswarm/beekeeper\n\ngo 1.26.0\n\ntoolchain go1.27.5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if a.hostPinToolsCurrent(*pin, a.cfg.Agents.Hosts[sshTestSelector]) {
		t.Fatal("pin at 1.27.4 reported current after repository bumped to 1.27.5")
	}
}
