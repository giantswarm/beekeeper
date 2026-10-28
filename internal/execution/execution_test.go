package execution

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/platform"
)

const (
	fixtureClaudeExecutable = "/usr/local/bin/claude"
	fixtureWorker           = "worker"
	fixtureHost             = "host-a"
	fixtureCoordinator      = "coordinator"
)

type fakeLauncher struct {
	mu        sync.Mutex
	starts    []platform.Unit
	stops     []string
	states    map[string]string
	stopError bool
}

func (f *fakeLauncher) Available() bool { return true }
func (f *fakeLauncher) Start(u platform.Unit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, u)
	f.states[u.Name] = unitActive
	return nil
}
func (f *fakeLauncher) State(_ context.Context, n string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.states[n]
}
func (f *fakeLauncher) Stop(_ context.Context, n string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, n)
	if f.stopError {
		return errors.New("transport lost")
	}
	f.states[n] = unitInactive
	return nil
}
func (f *fakeLauncher) Freeze(context.Context, string) error              { return nil }
func (f *fakeLauncher) Thaw(context.Context, string) error                { return nil }
func (f *fakeLauncher) Resume(context.Context, string) error              { return errors.New("not an owned start") }
func (f *fakeLauncher) Running(context.Context, bool, ...string) []string { return nil }
func (f *fakeLauncher) Failed(context.Context, ...string) []string        { return nil }
func (f *fakeLauncher) ResetFailed(context.Context, ...string) error      { return nil }

type fixture struct {
	s        *Server
	r        StartRequest
	key      ed25519.PrivateKey
	launcher *fakeLauncher
	host     Host
	source   string
}

func testFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "checkout")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "Example"}, {"config", "user.email", "example@example.invalid"}} {
		if _, err := hostCommand(ctx, source, os.Environ(), "git", args...); err != nil {
			t.Fatal("initializing fixture repository", err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "tracked.txt"}, {"commit", "-m", "initial fixture"}, {"remote", "add", "origin", "https://github.com/giantswarm/beekeeper.git"}} {
		if _, err := hostCommand(ctx, source, os.Environ(), "git", args...); err != nil {
			t.Fatal("preparing fixture repository", err)
		}
	}
	key, err := SigningKey(filepath.Join(root, fixtureCoordinator))
	if err != nil {
		t.Fatal(err)
	}
	callback := Endpoint{SSH: "coordinator.example", Executable: "/usr/local/bin/beekeeper", Config: "/etc/beekeeper/config.yaml"}
	cfg := HostConfig{Identity: fixtureHost, WorkspaceRoot: filepath.Join(root, "workspaces"), Repositories: map[string]string{"giantswarm/beekeeper": source}, Harnesses: map[string]HarnessConfig{HarnessClaude: {Executable: fixtureClaudeExecutable, Version: "2.1.290", ConfigDir: "/srv/beekeeper/auth"}}, Coordinators: map[string]Coordinator{fixtureCoordinator: {PublicKey: PublicKey(key), Callback: callback}}, MaxBusy: 2}
	launcher := &fakeLauncher{states: map[string]string{"personal.service": unitActive}}
	cfg.RulesFile = filepath.Join(root, "host-rules.md")
	if err := os.WriteFile(cfg.RulesFile, []byte("fixture host rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &Server{Config: cfg, Root: filepath.Join(root, "execution"), Executable: "/usr/local/bin/beekeeper", ConfigPath: "/etc/beekeeper/worker.yaml", Version: "test", SecurityDigest: Digest([]byte("host guards")), Rules: []byte("embedded worker rules"), Launcher: launcher, Now: func() time.Time { return now }, GuardProbe: func() error { return nil }, Authorize: func(context.Context, StartRequest) error { return nil }, Resources: func(context.Context) (Resources, error) {
		return Resources{AvailableMiB: 8192, DiskFreeGiB: 100, ObservedAt: now}, nil
	}}
	s.Command = func(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
		if bin == fixtureClaudeExecutable {
			if len(args) > 1 && args[0] == authCommand {
				return []byte(`{"loggedIn":true,"configDirectory":"/srv/beekeeper/auth","authMethod":"claude.ai"}`), nil
			}
			return []byte("2.1.290 (Claude Code)"), nil
		}
		return hostCommand(ctx, dir, env, bin, args...)
	}
	digest, err := s.Prepare(s.Policy())
	if err != nil {
		t.Fatal(err)
	}
	control, err := NewControlKey()
	if err != nil {
		t.Fatal(err)
	}
	r := StartRequest{Ref: Ref{Host: fixtureHost, Execution: ExecutionID(uuid.NewString()), Generation: 1}, RequestID: uuid.NewString(), Coordinator: fixtureCoordinator, Name: fixtureWorker, Task: "fixture task", Repository: "giantswarm/beekeeper", GitRef: "main", Harness: HarnessClaude, Model: "opus", Prompt: "complete the fixture task", PolicyDigest: digest, ConversationID: uuid.NewString(), ControlKey: control}
	host := Host{Endpoint: Endpoint{SSH: "host-a.example", Executable: s.Executable, Config: s.ConfigPath}, Identity: cfg.Identity, WorkspaceRoot: cfg.WorkspaceRoot, Repositories: cfg.Repositories, Harnesses: []string{HarnessClaude}, PolicyDigest: digest, MaxBusy: 2}
	return fixture{s: s, r: r, key: key, launcher: launcher, host: host, source: source}
}

func (f fixture) transport(t *testing.T, after func(WireRequest) error) SSHTransport {
	t.Helper()
	return SSHTransport{Host: f.host, Run: func(ctx context.Context, argv []string, b []byte) ([]byte, error) {
		for _, want := range []string{"BatchMode=yes", "StrictHostKeyChecking=yes", "ForwardAgent=no", "ClearAllForwardings=yes", "SendEnv=-*"} {
			if !strings.Contains(strings.Join(argv, " "), want) {
				t.Error("missing SSH security setting", want)
			}
		}
		if strings.Contains(argv[len(argv)-1], f.r.Prompt) {
			t.Error("prompt interpolated into shell command")
		}
		var req WireRequest
		if err := Decode(b, &req); err != nil {
			return nil, err
		}
		var out bytes.Buffer
		if err := f.s.Serve(ctx, bytes.NewReader(b), &out); err != nil {
			return nil, err
		}
		if after != nil {
			if err := after(req); err != nil {
				return nil, err
			}
		}
		return out.Bytes(), nil
	}}
}

func TestSSHStartRegistersBeforeActivationAndOwnsWorkspace(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	tr := f.transport(t, nil)
	if err := os.WriteFile(filepath.Join(f.source, "tracked.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.source, "untracked.txt"), []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := f.s.git(ctx, f.source, "status", "--porcelain")
	h, err := tr.Start(ctx, f.r)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.launcher.starts) != 0 {
		t.Fatal("harness started before registration receipt")
	}
	if h.Unit != UnitName(f.r.Ref) || h.ConversationID != f.r.ConversationID || h.Commit == "" || !within(f.s.Config.WorkspaceRoot, h.Workspace) {
		t.Fatal("invalid stable handle")
	}
	got, err := os.ReadFile(filepath.Join(h.Workspace, "tracked.txt"))
	if err != nil || string(got) != "original\n" {
		t.Fatal("clone reused uncommitted source work")
	}
	after, _ := f.s.git(ctx, f.source, "status", "--porcelain")
	if !bytes.Equal(before, after) {
		t.Fatal("source work changed")
	}
	f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
	if _, err := tr.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	if len(f.launcher.starts) != 1 {
		t.Fatal("expected one owned unit")
	}
	u := f.launcher.starts[0]
	if u.KeepChildren || !strings.Contains(strings.Join(u.Env, " "), EnvID+"="+string(f.r.Ref.Execution)) {
		t.Fatal("unit ownership environment or child lifetime is wrong")
	}
	if _, err := tr.Message(ctx, f.r.Ref, Envelope{}); !errors.Is(err, ErrUnsupported) {
		t.Fatal("remote wake did not explicitly refuse")
	}
}

func TestInterruptedActivationNeverStartsASecondProcess(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	tr := f.transport(t, nil)
	if _, err := tr.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
	drop := f.transport(t, func(req WireRequest) error {
		if req.Operation == "start" {
			return errors.New("reply interrupted")
		}
		return nil
	})
	if _, err := drop.Start(ctx, f.r); err == nil {
		t.Fatal("lost reply was reported successful")
	}
	if _, err := tr.Start(ctx, f.r); err == nil {
		t.Fatal("uncertain activation was silently relaunched")
	}
	if len(f.launcher.starts) != 1 {
		t.Fatal("duplicate process")
	}
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error { j.Phase, j.Ready = Running, true; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Start(ctx, f.r); err != nil {
		t.Fatal("existing accepted process was not recovered", err)
	}
	changed := f.r
	changed.Prompt = "different task"
	if _, err := tr.Start(ctx, changed); err == nil {
		t.Fatal("conflicting idempotency key accepted")
	}
	if len(f.launcher.starts) != 1 {
		t.Fatal("recovery launched another process")
	}
}

func TestMissingStaleAndWrongGenerationReceiptsRefuse(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	stale := f.r
	stale.RegistrationReceipt = Sign(stale, f.key, f.s.now().Add(-2*time.Minute))
	if _, err := f.s.Start(ctx, stale); err == nil {
		t.Fatal("stale receipt accepted")
	}
	wrong := f.r
	wrong.Ref.Generation++
	wrong.RegistrationReceipt = Sign(wrong, f.key, f.s.now())
	if _, err := f.s.Start(ctx, wrong); err == nil {
		t.Fatal("wrong generation accepted")
	}
	f.s.Authorize = func(context.Context, StartRequest) error { return errors.New("coordinator is stopped") }
	f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
	if _, err := f.s.Start(ctx, f.r); err == nil {
		t.Fatal("activation ignored the coordinator stop")
	}
	if len(f.launcher.starts) != 0 {
		t.Fatal("unregistered or stale-authority process started")
	}
}

func TestStopPersistsIntentAndNeverTouchesPersonalSessions(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	f.launcher.stopError = true
	q := StopRequest{ID: uuid.NewString(), Reason: "fixture stop"}
	if _, err := f.s.Stop(ctx, f.r.Ref, q); err == nil {
		t.Fatal("failed stop was acknowledged")
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil || j.StopRequest == nil || j.StopAcknowledged {
		t.Fatal("stop intent was lost or falsely acknowledged")
	}
	f.launcher.stopError = false
	ack, err := f.s.Stop(ctx, f.r.Ref, q)
	if err != nil || ack.Stage != Applied {
		t.Fatal("stop did not reconcile", err)
	}
	if _, err := f.s.Stop(ctx, f.r.Ref, q); err != nil {
		t.Fatal(err)
	}
	if len(f.launcher.stops) != 2 || f.launcher.states["personal.service"] != unitActive {
		t.Fatal("stop repeated after acknowledgement or touched a personal unit")
	}
	bad := f.r.Ref
	bad.Generation++
	if _, err := f.s.Stop(ctx, bad, q); err == nil {
		t.Fatal("stale generation stopped a unit")
	}
}

func TestPolicyAndTransportFailuresAreExplicit(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	tr := f.transport(t, nil)
	if _, err := tr.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.s.Root, "settings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Start(ctx, f.r); err == nil || len(f.launcher.starts) != 0 {
		t.Fatal("missing guard installation allowed launch")
	}
	tr.Run = func(context.Context, []string, []byte) ([]byte, error) { return nil, errors.New("network interrupted") }
	_, err := tr.Inspect(ctx, f.r.Ref)
	var down *UnreachableError
	if !errors.As(err, &down) {
		t.Fatal("reachability was not distinguished from process state")
	}
	tr.Host.SSH = "-oProxyCommand=unsafe"
	if _, err := tr.Check(ctx); err == nil {
		t.Fatal("SSH option injection accepted")
	}
}

func TestLogsUseOwnedBoundedCursors(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.s.Root, string(f.r.Ref.Execution)+".log"), []byte("first\nsecond\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	page, err := f.s.Logs(ctx, f.r.Ref, LogRequest{Limit: 6})
	if err != nil || string(page.Data) != "first\n" || page.Next != "6" || !page.Truncated {
		t.Fatal("incorrect log page", err)
	}
	page, err = f.s.Logs(ctx, f.r.Ref, LogRequest{After: page.Next, Limit: 16})
	if err != nil || string(page.Data) != "second\n" {
		t.Fatal("incorrect resumed logs", err)
	}
	if _, err := f.s.Logs(ctx, f.r.Ref, LogRequest{After: "../another-log"}); err == nil {
		t.Fatal("arbitrary cursor accepted")
	}
}

func TestCallerUsesServiceIdentityAndCallbackProof(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	f.s.OwnUnit = func() string { return "personal.service" }
	if _, err := f.s.Caller(string(f.r.Ref.Execution)); err == nil {
		t.Fatal("environment alone impersonated an execution")
	}
	f.s.OwnUnit = func() string { return UnitName(f.r.Ref) }
	j, err := f.s.Caller(string(f.r.Ref.Execution))
	if err != nil || j.Request.Name != fixtureWorker {
		t.Fatal("owned service identity was not preserved")
	}
	r := CallbackRequest{WireRequest: WireRequest{Protocol: Protocol, RequestID: uuid.NewString(), Ref: f.r.Ref, Operation: "control", Payload: json.RawMessage(`{}`)}}
	r.Proof = callbackProof(r.WireRequest, f.r.ControlKey)
	if !VerifyCallback(r, f.r.ControlKey) {
		t.Fatal("valid callback proof rejected")
	}
	r.Ref.Generation++
	if VerifyCallback(r, f.r.ControlKey) {
		t.Fatal("callback proof was not generation-bound")
	}
}
