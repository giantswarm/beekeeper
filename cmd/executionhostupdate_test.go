package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
)

// The releases of the host update tests: the host's and the coordinator's.
const (
	hostOldRelease = "v0.5.0"
	hostNewRelease = "v0.6.0"
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
	endpoint := execution.Endpoint{SSH: "coordinator.example", Executable: sshTestExecutable, Config: sshTestConfig}
	s := &execution.Server{Root: filepath.Join(root, "execution"), Rules: []byte("fixture rules"), Version: version, Executable: sshTestExecutable, ConfigPath: "/etc/beekeeper/host.yaml", Config: execution.HostConfig{Identity: sshTestSelector, RulesFile: filepath.Join(root, "host-rules.md"), WorkspaceRoot: filepath.Join(root, "workspaces"), Repositories: map[string]string{sshTestRepository: filepath.Join(root, "source")}, Harnesses: map[string]execution.HarnessConfig{claudeComm: {Executable: "/usr/local/bin/claude", Version: "2.1.290"}}, Coordinators: map[string]execution.Coordinator{sshTestCoordinator: {PublicKey: execution.PublicKey(key), Callback: endpoint}}, MaxBusy: 1}}
	if _, err := s.Prepare(s.Policy()); err != nil {
		t.Fatal(err)
	}
	a.executionRuntime = func() (*execution.Server, error) { return s, nil }
	return s
}

// hostInstall replaces the host's install, its rollback and the installed
// release's prepare: install records its calls, prepare answers answer.
type hostInstall struct {
	installs, rollbacks []string
	prepareArgs         []string
	answer              execution.HostUpdate
}

func stubHostInstall(t *testing.T, h *hostInstall) {
	t.Helper()
	install, rollback, prepare := installVersion, rollbackInstall, runHostPrepare
	t.Cleanup(func() { installVersion, rollbackInstall, runHostPrepare = install, rollback, prepare })
	installVersion = func(_ context.Context, _ io.Writer, version string) (update.Result, error) {
		h.installs = append(h.installs, version)
		return update.Result{Latest: version, Path: sshTestExecutable, Updated: true}, nil
	}
	rollbackInstall = func(path string) error {
		h.rollbacks = append(h.rollbacks, path)
		return nil
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
	got := a.hostSelfUpdate(context.Background(), execution.Protocol, hostNewRelease)
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
	got := a.hostSelfUpdate(context.Background(), execution.Protocol, hostNewRelease)
	if got != want {
		t.Fatalf("self-update answered %+v, want the installed release's %+v", got, want)
	}
	if !slices.Equal(h.installs, []string{hostNewRelease}) || len(h.rollbacks) != 0 {
		t.Errorf("installs %v, rollbacks %v", h.installs, h.rollbacks)
	}
	wantArgs := []string{sshTestExecutable, "--" + configFlag, s.ConfigPath, agentsName, "hosts", "self-update", "--protocol", "1", "--version", hostNewRelease}
	if !slices.Equal(h.prepareArgs, wantArgs) {
		t.Errorf("the installed release ran %v, want %v", h.prepareArgs, wantArgs)
	}
}

func TestHostSelfUpdateRollsBackAReleaseThatDoesNotPrepare(t *testing.T) {
	for name, answer := range map[string]execution.HostUpdate{
		"error":     {Protocol: execution.Protocol, Identity: sshTestSelector, Version: hostNewRelease, Error: "policy bundle differs"},
		"no digest": {Protocol: execution.Protocol, Identity: sshTestSelector, Version: hostNewRelease},
		"other":     {Protocol: execution.Protocol, Identity: sshTestSelector, Version: hostOldRelease, PolicyDigest: execution.Digest([]byte("x"))},
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
			got := a.hostSelfUpdate(context.Background(), execution.Protocol, hostNewRelease)
			if !strings.Contains(got.Error, "rolled back to "+hostOldRelease) || got.PolicyDigest != "" {
				t.Fatalf("self-update answered %+v, want a rollback", got)
			}
			if !slices.Equal(h.rollbacks, []string{sshTestExecutable}) {
				t.Errorf("rollbacks %v", h.rollbacks)
			}
			if _, err := s.InstalledPolicy(); err != nil {
				t.Errorf("the rolled back release's policy is not prepared again: %v", err)
			}
		})
	}
}

func TestHostSelfUpdateChangesNothingWhileExecutionsRun(t *testing.T) {
	a, r := executionTestApp(t)
	s := hostUpdateServer(t, a, hostOldRelease)
	j := execution.Journal{Phase: execution.Running, Handle: execution.Handle{Ref: r.Ref}}
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, uuid.NewString()+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	h := &hostInstall{}
	stubHostInstall(t, h)
	got := a.hostSelfUpdate(context.Background(), execution.Protocol, hostNewRelease)
	if got.Busy != 1 || got.Error != "" || got.PolicyDigest != "" || got.Version != hostOldRelease {
		t.Fatalf("self-update answered %+v, want busy and unchanged", got)
	}
	if len(h.installs) != 0 {
		t.Errorf("installed %v while an execution runs", h.installs)
	}
}

func TestHostSelfUpdateRefusesAWorkerOrAnotherProtocol(t *testing.T) {
	a, r := executionTestApp(t)
	hostUpdateServer(t, a, hostOldRelease)
	h := &hostInstall{}
	stubHostInstall(t, h)
	for name, run := range map[string]func() execution.HostUpdate{
		"protocol": func() execution.HostUpdate {
			return a.hostSelfUpdate(context.Background(), execution.Protocol+1, hostNewRelease)
		},
		"version": func() execution.HostUpdate {
			return a.hostSelfUpdate(context.Background(), execution.Protocol, "latest")
		},
		"worker": func() execution.HostUpdate {
			t.Setenv(execution.EnvID, string(r.Ref.Execution))
			return a.hostSelfUpdate(context.Background(), execution.Protocol, hostNewRelease)
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

// hostUpdates answers the coordinator's SSH update of each host.
func stubHostUpdates(a *app, answers map[execution.HostID]func(string) (execution.HostUpdate, error)) *[]string {
	var calls []string
	a.executionUpdate = func(_ context.Context, h execution.Host, version string) (execution.HostUpdate, error) {
		calls = append(calls, string(h.Identity)+" "+version)
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
	stubHostUpdates(a, map[execution.HostID]func(string) (execution.HostUpdate, error){sshTestSelector: pinnedAnswer(sshTestSelector, digest)})
	var probed []string
	probe := a.executionProbe
	a.executionProbe = func(ctx context.Context, h execution.Host) (execution.Capabilities, error) {
		probed = append(probed, h.PolicyDigest)
		return probe(ctx, h)
	}
	out, err := runAgents(a, "hosts", hostUpdateCmdName, sshTestSelector)
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
	// The coordinator moves on: the host takes no start until it follows.
	withCoordinatorVersion(t, "v0.7.0")
	if err := a.admitExecution(sshTestSelector, got); Code(err) != ExitRefused || !strings.Contains(err.Error(), "v0.7.0") {
		t.Errorf("a host behind the coordinator: %v", err)
	}
}

func TestHostsUpdateHoldsAFailingHostWhileTheOthersRun(t *testing.T) {
	a, _ := sshStartTestApp(t)
	withCoordinatorVersion(t, hostNewRelease)
	digest := execution.Digest([]byte("prepared on host-b"))
	failing := true
	stubHostUpdates(a, map[execution.HostID]func(string) (execution.HostUpdate, error){
		sshTestSelector: func(version string) (execution.HostUpdate, error) {
			if failing {
				return execution.HostUpdate{}, errors.New("no signed release for this platform")
			}
			return pinnedAnswer(sshTestSelector, digest)(version)
		},
		sshTestOtherSelector: pinnedAnswer(sshTestOtherSelector, digest),
	})
	if _, err := runAgents(a, "hosts", hostUpdateCmdName, sshTestSelector); Code(err) != ExitRefused || !strings.Contains(err.Error(), "held") {
		t.Fatalf("a failing update: %v", err)
	}
	if _, err := runAgents(a, "hosts", hostUpdateCmdName, sshTestOtherSelector); err != nil {
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
	if _, err := runAgents(a, agentStartName, "--name", "remote-worker", brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--model", bgModel); Code(err) != ExitRefused {
		t.Errorf("a start on a held host: %v", err)
	}
	if st, _ := a.store.Read(); len(st.Starts) != 0 {
		t.Errorf("a start on a held host reserved %d", len(st.Starts))
	}
	failing = false
	if _, err := runAgents(a, "hosts", hostUpdateCmdName, sshTestSelector); err != nil {
		t.Fatal(err)
	}
	if st, _ := a.store.Read(); st.HostPin(sshTestSelector).Held != "" {
		t.Errorf("a successful update left the hold: %+v", st.HostPin(sshTestSelector))
	}
}

func TestHostsUpdateDrainsAHostWithExecutions(t *testing.T) {
	a, _ := sshStartTestApp(t)
	withCoordinatorVersion(t, hostNewRelease)
	calls := stubHostUpdates(a, map[execution.HostID]func(string) (execution.HostUpdate, error){
		sshTestSelector: func(string) (execution.HostUpdate, error) {
			return execution.HostUpdate{Protocol: execution.Protocol, Identity: sshTestSelector, Version: hostOldRelease, Busy: 2}, nil
		},
	})
	if _, err := runAgents(a, "hosts", hostUpdateCmdName, sshTestSelector); Code(err) != ExitRefused || !strings.Contains(err.Error(), "drains") {
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
		st.Starts = append(st.Starts, state.Start{Party: state.Party{Name: "remote-worker"}, Execution: &state.ExecutionRecord{Selector: sshTestSelector, Handle: execution.Handle{Ref: ref}, Phase: execution.Running, Observation: execution.Observation{ProcessState: execution.Running}}})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	*calls = nil
	if _, err := runAgents(a, "hosts", hostUpdateCmdName, sshTestSelector); Code(err) != ExitRefused || len(*calls) != 0 {
		t.Fatalf("recorded executions: %v, calls %v", err, *calls)
	}
}

func TestHostsUpdateRefusesADevelopmentBuildWhichGatesNoStart(t *testing.T) {
	a, _ := sshStartTestApp(t)
	withCoordinatorVersion(t, "dev")
	calls := stubHostUpdates(a, nil)
	if _, err := runAgents(a, "hosts", hostUpdateCmdName, sshTestSelector); Code(err) != ExitRefused || len(*calls) != 0 {
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
	for _, selector := range []string{sshTestSelector, sshTestOtherSelector, "host-c"} {
		r.a.cfg.Agents.Hosts[selector] = execution.Host{Endpoint: execution.Endpoint{SSH: selector + ".example", Executable: sshTestExecutable, Config: sshTestConfig}, Identity: execution.HostID(selector), WorkspaceRoot: "/srv/workspaces", Repositories: map[string]string{sshTestRepository: "/srv/source"}, Harnesses: []string{claudeComm}, MaxBusy: 1}
	}
	ref := execution.Ref{Host: "host-c", Execution: execution.ExecutionID(uuid.NewString()), Generation: 1}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{Party: state.Party{Name: "remote-worker"}, Execution: &state.ExecutionRecord{Selector: "host-c", Handle: execution.Handle{Ref: ref}, Phase: execution.Running}})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	// The command records what agents hosts update records.
	r.answer = func(args []string) (string, int) {
		if len(args) != 4 || args[0] != agentsName || args[1] != "hosts" || args[2] != hostUpdateCmdName {
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
	l := &supervisorLoop{a: r.a, said: map[string]bool{}}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	l.updateHosts(context.Background(), st)
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
	if strings.Count(out, "SUPERVISOR HOST HELD") != 1 || !strings.Contains(out, "SUPERVISOR HOST UPDATED "+sshTestOtherSelector) || !strings.Contains(out, "SUPERVISOR HOST DRAINS host-c") {
		t.Fatalf("loop said:\n%s", out)
	}
	findings, err := r.a.store.Events(0, func(e state.Event) bool { return e.Verb == "supervisor.file" })
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || !strings.Contains(findings[0].Detail, fileReportSeparator) || !strings.Contains(findings[0].Detail, "unreachable") {
		t.Fatalf("findings %+v", findings)
	}
	// A later pass within the retry interval updates nothing again and
	// hands the hold on no second time.
	r.calls = nil
	r.a.now = r.a.now.Add(time.Minute)
	st, _ = r.a.store.Read()
	l.updateHosts(context.Background(), st)
	for _, c := range r.calls {
		if len(c) == 4 && c[2] == hostUpdateCmdName {
			t.Errorf("updated %v again", c)
		}
	}
	if findings, _ := r.a.store.Events(0, func(e state.Event) bool { return e.Verb == "supervisor.file" }); len(findings) != 1 {
		t.Errorf("%d findings, want the one", len(findings))
	}
	// An hour on, the held host is tried again.
	r.calls = nil
	r.a.now = r.a.now.Add(hostUpdateRetry)
	st, _ = r.a.store.Read()
	l.updateHosts(context.Background(), st)
	if len(r.calls) == 0 || r.calls[0][3] != sshTestSelector {
		t.Errorf("the held host was not tried again: %v", r.calls)
	}
}
