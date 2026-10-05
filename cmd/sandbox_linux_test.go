package cmd

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/sandbox"
)

// The broker serves on Linux only.

func TestBrokeredSecretRunsAsTheRequester(t *testing.T) {
	bin := t.TempDir()
	exe := filepath.Join(bin, "beekeeper")
	script := "#!/bin/sh\necho \"args=$* pwd=$(pwd) session=$CLAUDE_CODE_SESSION_ID brokered=$" + sandbox.Brokered + " other=$BEEKEEPER_TEST_OTHER\"\necho warned >&2\nexit 3\n"
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil { //nolint:gosec // the test's fake binary
		t.Fatal(err)
	}
	// a shell that says when it runs and stays the process (no tail exec):
	// Start returns before the child's environment is in place
	requester := exec.Command("/bin/sh", "-c", "echo ready; /bin/sleep 5; true")
	requester.Dir = t.TempDir()
	requester.Env = []string{"CLAUDE_CODE_SESSION_ID=s-1", "BEEKEEPER_TEST_OTHER=leaked", "PATH=/usr/bin:/bin"}
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
	h := brokered(brokeredCap(&recordingCapper{}), map[string]sandbox.Handler{sandbox.OpSecret: brokeredCall(exe, "/proc", brokeredCallTimeout, nil, brokeredSecretArgv)})
	r, err := h(context.Background(), requester.Process.Pid, sandbox.Request{Op: sandbox.OpSecret, Args: []string{compareOp, sopsA, sopsB}})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := filepath.EvalSymlinks(requester.Dir)
	want := "args=secret compare a.sops.yaml b.sops.yaml pwd=" + dir + " session=s-1 brokered=1 other=\n"
	if r.Out != want || r.Err != "warned\n" || r.Code != 3 {
		t.Errorf("reply %+v, want out %q", r, want)
	}
	if _, err := h(context.Background(), requester.Process.Pid, sandbox.Request{Op: sandbox.OpSecret, Args: []string{"setup"}}); err == nil {
		t.Error("setup: want a refusal")
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
	if err := a.gate(context.Background(), argv, time.Minute, false); Code(err) != 9 {
		t.Errorf("exit %d (%v), want the brokered gate's 9", Code(err), err)
	}
	if len(got) != 1 || got[0].Op != sandbox.OpGate || strings.Join(got[0].Args, " ") != strings.Join(argv, " ") || got[0].Wait != "1m0s" {
		t.Errorf("broker got %+v", got)
	}
	if err := a.gate(context.Background(), argv, time.Minute, true); err == nil {
		t.Error("a queued run from the sandbox: want a refusal")
	}
}
