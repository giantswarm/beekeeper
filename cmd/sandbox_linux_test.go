package cmd

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// The broker serves on Linux only.

func TestBrokeredSecretRunsAsTheRequester(t *testing.T) {
	skipWithoutOwnProc(t)
	bin := t.TempDir()
	exe := filepath.Join(bin, "beekeeper")
	script := "#!/bin/sh\necho \"args=$* pwd=$(pwd) session=$CLAUDE_CODE_SESSION_ID brokered=$" + sandbox.Brokered + " other=$BEEKEEPER_TEST_OTHER\"\necho warned >&2\nexit 3\n"
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil { //nolint:gosec // the test's fake binary
		t.Fatal(err)
	}
	requesterDir := t.TempDir()
	pid := startRequester(t, requesterDir, []string{"CLAUDE_CODE_SESSION_ID=s-1", "BEEKEEPER_TEST_OTHER=leaked", "PATH=/usr/bin:/bin"})
	h := brokered(brokeredCap(&recordingCapper{}, platform.Cap{}), map[string]sandbox.Handler{sandbox.OpSecret: brokeredCall(exe, "/proc", brokeredCallTimeout, nil, brokeredSecretArgv(false))})
	r, err := h(context.Background(), pid, sandbox.Request{Op: sandbox.OpSecret, Args: []string{compareOp, sopsA, sopsB}})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := filepath.EvalSymlinks(requesterDir)
	want := "args=secret compare a.sops.yaml b.sops.yaml pwd=" + dir + " session=s-1 brokered=1 other=\n"
	if r.Out != want || r.Err != "warned\n" || r.Code != 3 {
		t.Errorf("reply %+v, want out %q", r, want)
	}
	if _, err := h(context.Background(), pid, sandbox.Request{Op: sandbox.OpSecret, Args: []string{"setup"}}); err == nil {
		t.Error("setup: want a refusal")
	}
}

// startRequester starts a process in dir, with env when given, that holds
// a request for the test's broker, and answers its process id.
func startRequester(t *testing.T, dir string, env []string) int {
	t.Helper()
	// a shell that says when it runs and stays the process (no tail exec):
	// Start returns before the child's environment is in place
	requester := exec.Command("/bin/sh", "-c", "echo ready; /bin/sleep 5; true")
	requester.Dir, requester.Env = dir, env
	ready, err := requester.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := requester.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = requester.Process.Kill(); _ = requester.Wait() })
	if _, err := bufio.NewReader(ready).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	return requester.Process.Pid
}

func TestBrokeredCallActsOnTheRequesterCheckout(t *testing.T) {
	skipWithoutOwnProc(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	checkout := gitCheckout(t, "https://git.example/caller.git")
	bin := t.TempDir()
	exe := filepath.Join(bin, "beekeeper")
	// the brokered call's consumer, a relative path in its argv
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexec git -C . remote get-url origin\n"), 0o700); err != nil { //nolint:gosec // the test's fake binary
		t.Fatal(err)
	}
	pid := startRequester(t, checkout, nil)
	h := brokeredCall(exe, "/proc", brokeredCallTimeout, nil, brokeredSecretArgv(false))
	r, err := h(context.Background(), pid, sandbox.Request{Op: sandbox.OpSecret, Args: []string{copyOp, sopsA, "--", "git", "-C", ".", "ls-remote", "origin"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Out != "https://git.example/caller.git\n" || r.Code != 0 {
		t.Errorf("reply %+v, want the requester checkout's origin", r)
	}
}

func TestSecretInTheSandboxGoesThroughTheBroker(t *testing.T) {
	a, _, repo := secretApp(t)
	t.Setenv(sandbox.Env, "1")
	var got [][]string
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sandbox.Serve(ctx, sandbox.SpoolDir(a.cfg.StateDir), "/proc", 5*time.Millisecond, func(_ context.Context, _ int, req sandbox.Request) (sandbox.Reply, error) {
			if req.Op == sandbox.OpSecret {
				got = append(got, req.Args)
				return sandbox.Reply{Out: "brokered\n", Code: ExitError}, nil
			}
			return sandbox.Reply{}, nil
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	src := filepath.Join(repo, "db.sops.yaml")
	out, err := runSecret(a, copyOp, "--name=other", src, filepath.Join(repo, "x.sops.yaml"))
	if out != "brokered\n" || Code(err) != ExitError {
		t.Errorf("out %q, exit %d", out, Code(err))
	}
	if want := []string{copyOp, "--name=other", src, filepath.Join(repo, "x.sops.yaml")}; len(got) != 1 || strings.Join(got[0], " ") != strings.Join(want, " ") {
		t.Errorf("broker got %q, want %q", got, want)
	}
	if _, err := runSecret(a, copyOp, src+"#data.password", "--", "sh", "-c", "cat"); Code(err) != ExitRefused {
		t.Errorf("a consumer: exit %d, want refused", Code(err))
	}
	if _, err := runSecret(a, "setup"); Code(err) != ExitRefused {
		t.Errorf("setup: exit %d, want refused", Code(err))
	}
	if len(got) != 1 {
		t.Errorf("a refused call reached the broker: %q", got)
	}
}

func TestGateInTheSandboxGoesThroughTheBroker(t *testing.T) {
	a, _, _ := secretApp(t)
	t.Setenv(sandbox.Env, "1")
	var got []sandbox.Request
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sandbox.Serve(ctx, sandbox.SpoolDir(a.cfg.StateDir), "/proc", 5*time.Millisecond, func(_ context.Context, _ int, req sandbox.Request) (sandbox.Reply, error) {
			if req.Op != sandbox.OpGate {
				return sandbox.Reply{}, nil
			}
			got = append(got, req)
			return sandbox.Reply{Out: "{\"merged\":true}\n", Code: 9}, nil
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	argv := strings.Fields("devctl pr merge giantswarm/beekeeper 7")
	if err := a.gate(context.Background(), argv, time.Minute, 0, false); Code(err) != 9 {
		t.Errorf("exit %d (%v), want the brokered gate's 9", Code(err), err)
	}
	if len(got) != 1 || got[0].Op != sandbox.OpGate || strings.Join(got[0].Args, " ") != strings.Join(argv, " ") || got[0].Wait != "1m0s" {
		t.Errorf("broker got %+v", got)
	}
	if err := a.gate(context.Background(), argv, time.Minute, 0, true); err == nil {
		t.Error("a queued run from the sandbox: want a refusal")
	}
}

// hostBroker serves the app's spool with h until the test ends.
func hostBroker(t *testing.T, a *app, h sandbox.Handler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sandbox.Serve(ctx, sandbox.SpoolDir(a.cfg.StateDir), "/proc", 5*time.Millisecond, h)
	}()
	t.Cleanup(func() { cancel(); <-done })
}

func TestRoleCommandsInTheSandboxGoThroughTheBroker(t *testing.T) {
	a, _, _ := secretApp(t)
	t.Setenv(sandbox.Env, "1")
	var got []sandbox.Request
	hostBroker(t, a, func(_ context.Context, _ int, req sandbox.Request) (sandbox.Reply, error) {
		if req.Op == sandbox.OpPing {
			return sandbox.Reply{}, nil
		}
		got = append(got, req)
		if w := req.Output(); w != nil {
			_, _ = io.WriteString(w, "streamed\n")
		}
		return sandbox.Reply{Code: 4}, nil
	})
	run := func(c *cobra.Command, args ...string) (string, error) {
		a.out = &bytes.Buffer{}
		usageArgs(c)
		c.SetArgs(args)
		c.SetOut(a.out)
		c.SetErr(a.out)
		c.SilenceUsage, c.SilenceErrors = true, true
		err := c.ExecuteContext(context.Background())
		return a.out.(*bytes.Buffer).String(), err
	}
	for _, tc := range []struct {
		c    *cobra.Command
		args []string
		op   string
		want string
	}{
		{a.agentsCmd(), []string{agentWakeName, agentBK1, "go on -- now"}, sandbox.OpAgents, "wake -- BK 1 go on -- now"},
		{a.agentsCmd(), []string{agentResumeName, agentBK1}, sandbox.OpAgents, "resume -- BK 1"},
		{a.agentsCmd(), []string{agentStartName, "--task", "t", "BK 2", briefMD}, sandbox.OpAgents, "start --task=t -- BK 2 " + briefMD},
		{a.watchCmd(), []string{"--notify"}, "", "watch --notify=true --"},
	} {
		if tc.op == "" {
			tc.c = a.onHost(tc.c, sandbox.OpWatch, 0)
			tc.op = sandbox.OpWatch
		}
		got = nil
		out, err := run(tc.c, tc.args...)
		if out != "streamed\n" || Code(err) != 4 {
			t.Errorf("%q: out %q, exit %d (%v)", tc.args, out, Code(err), err)
		}
		if len(got) != 1 || got[0].Op != tc.op || !got[0].Stream || strings.Join(got[0].Args, " ") != tc.want {
			t.Errorf("%q: broker got %+v, want %s %q", tc.args, got, tc.op, tc.want)
		}
	}
	got = nil
	root := &cobra.Command{Use: project.Name}
	root.PersistentFlags().StringVar(&a.as, "as", "", "")
	root.AddCommand(a.agentsCmd())
	if _, err := run(root, "agents", agentWakeName, "--as", "supervisor", agentBK1, "hi"); Code(err) != ExitRefused || len(got) != 0 {
		t.Errorf("--as: exit %d, broker got %+v; want refused here", Code(err), got)
	}
}

func TestBrokeredStartKeepsTheScratchState(t *testing.T) {
	skipWithoutOwnProc(t)
	bin := t.TempDir()
	exe := filepath.Join(bin, "beekeeper")
	script := "#!/bin/sh\necho \"args=$* state=$" + stateFromEnv + " config=$BEEKEEPER_CONFIG\"\n"
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil { //nolint:gosec // the test's fake binary
		t.Fatal(err)
	}
	// the broker's own environment names the host's configuration and no
	// scratch state; the requester's names its own, which stay its own
	t.Setenv("BEEKEEPER_CONFIG", "/host/config.yaml")
	t.Setenv(stateFromEnv, "/broker/leaked.yaml")
	requester := exec.Command("/bin/sh", "-c", "echo ready; /bin/sleep 5; true")
	requester.Env = []string{"CLAUDE_CODE_SESSION_ID=s-1", "BEEKEEPER_CONFIG=/requester/config.yaml", stateFromEnv + "=/requester/leaked.yaml", "PATH=/usr/bin:/bin"}
	ready, err := requester.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := requester.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(ready).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = requester.Process.Kill(); _ = requester.Wait() }()
	h := brokeredAgents(func(env []string) sandbox.Handler {
		return brokeredCall(exe, "/proc", agentsBrokeredTimeout, env, brokeredAgentsArgv)
	})
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{agentStartName, "--task=t", "--config=/scratch/bk.yaml", "--", agentBK1, briefMD}, "args=agents start --task=t -- BK 1 " + briefMD + " state=/scratch/bk.yaml config=/host/config.yaml\n"},
		{[]string{agentWakeName, "--", agentBK1, "hi"}, "args=agents wake -- BK 1 hi state= config=/host/config.yaml\n"},
	} {
		r, err := h(context.Background(), requester.Process.Pid, sandbox.Request{Op: sandbox.OpAgents, Args: tc.args})
		if err != nil || r.Out != tc.want || r.Code != 0 {
			t.Errorf("%q: reply %+v, %v; want out %q", tc.args, r, err, tc.want)
		}
	}
	for _, args := range [][]string{
		{agentStartName, "--config=scratch.yaml", "--", agentBK1, briefMD},
		{agentStartName, "--config", "/scratch/bk.yaml", "--", agentBK1, briefMD},
		{"remove", "--config=/scratch/bk.yaml", "--", agentBK1},
	} {
		if r, err := h(context.Background(), requester.Process.Pid, sandbox.Request{Op: sandbox.OpAgents, Args: args}); err == nil {
			t.Errorf("%q: reply %+v, want a refusal", args, r)
		}
	}
}

func TestScratchStartInTheSandboxAsksTheHostBroker(t *testing.T) {
	a, _, _ := secretApp(t)
	t.Setenv(sandbox.Env, "1")
	host := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(host, []byte("stateDir: "+a.cfg.StateDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEEKEEPER_CONFIG", host)
	var got []sandbox.Request
	// the host's broker serves the host's state, whatever the call names
	hostApp, hostCfg := *a, *a.cfg
	hostApp.cfg = &hostCfg
	hostBroker(t, &hostApp, func(_ context.Context, _ int, req sandbox.Request) (sandbox.Reply, error) {
		if req.Op != sandbox.OpPing {
			got = append(got, req)
		}
		return sandbox.Reply{}, nil
	})
	scratch := filepath.Join(t.TempDir(), "bk.yaml")
	for _, tc := range []struct{ flag, env string }{{flag: scratch}, {env: scratch}} {
		// the scratch configuration is this call's: its state has no broker
		a.cfgPath, a.cfg.StateDir = tc.flag, t.TempDir()
		t.Setenv(stateFromEnv, tc.env)
		got = nil
		c := a.agentsCmd()
		c.SetArgs([]string{agentStartName, "--task", "t", "BK 2", briefMD})
		c.SetOut(io.Discard)
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		want := "start --task=t --config=" + scratch + " -- BK 2 " + briefMD
		if len(got) != 1 || strings.Join(got[0].Args, " ") != want {
			t.Errorf("%+v: broker got %+v, want %q", tc, got, want)
		}
	}
}

// Store sources use the broker in the sandbox or with a vault session.
func TestStoreSourceUsesBrokerWhenRequired(t *testing.T) {
	const brokerStoreResponse = "brokered store\n"
	for _, tc := range []struct{ session, sandbox bool }{{true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			a, _, _ := secretApp(t)
			a.cfg.Secret.Session = tc.session
			a.cfg.Secret.Vault = ""
			t.Setenv(sandbox.Env, "")
			if tc.sandbox {
				t.Setenv(sandbox.Env, "1")
			}
			t.Setenv(sandbox.Brokered, "")
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = sandbox.Serve(ctx, sandbox.SpoolDir(a.cfg.StateDir), "/proc", 5*time.Millisecond, func(_ context.Context, _ int, req sandbox.Request) (sandbox.Reply, error) {
					if req.Op == sandbox.OpPing {
						return sandbox.Reply{}, nil
					}
					if req.Op != sandbox.OpSecret {
						return sandbox.Reply{}, fmt.Errorf("unexpected broker operation %s", req.Op)
					}
					return sandbox.Reply{Out: brokerStoreResponse}, nil
				})
			}()
			t.Cleanup(func() { cancel(); <-done })
			out, err := runSecret(a, fingerprintOp, noBrokerStoreRef)
			if err != nil || out != brokerStoreResponse {
				t.Fatalf("store did not reach broker: %q, %v", out, err)
			}
			if tc.session && !tc.sandbox {
				a.out = &bytes.Buffer{}
				out, err = runSecret(a, fingerprintOp, "--", noBrokerStoreRef)
				if err != nil || out != brokerStoreResponse {
					t.Fatalf("flag separator lost store routing: %q, %v", out, err)
				}
			}
		})
	}
}
