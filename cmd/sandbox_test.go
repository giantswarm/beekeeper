package cmd

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/sandbox"
)

// The broker test's scope, slice and cap.
const (
	testRunUnit     = "memcap-4242-000123"
	testMemcapSlice = "memcap.slice"
	testMax         = "12G"
)

// recordingCapper records what the broker asks of the host.
type recordingCapper struct {
	slots  []platform.Cap
	adopts []int
}

func (*recordingCapper) Available() bool { return true }
func (*recordingCapper) Capped() bool    { return false }
func (r *recordingCapper) CapSlot(c platform.Cap) error {
	r.slots = append(r.slots, c)
	return nil
}
func (*recordingCapper) Command(string, platform.Cap, []string) (*exec.Cmd, error) { return nil, nil }
func (r *recordingCapper) Adopt(pid int, _ string, _ platform.Cap) error {
	r.adopts = append(r.adopts, pid)
	return nil
}

func TestBrokeredCap(t *testing.T) {
	r := &recordingCapper{}
	h := brokeredCap(r)
	for _, ok := range []sandbox.Request{
		{Op: sandbox.OpPing},
		{Op: sandbox.OpCapSlot, Slice: "memcap-slot1_0a1b2c3d.slice", Max: testMax, Swap: "0"},
		{Op: sandbox.OpScope, Unit: testRunUnit, Slice: "memcap-slot2_test.slice", Max: "infinity", Swap: "0"},
		{Op: sandbox.OpScope, Unit: "memcap-test-4242-000123", Slice: testMemcapSlice, Max: "512M", Swap: "0"},
	} {
		if err := h(7, ok); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	if len(r.slots) != 1 || len(r.adopts) != 2 || r.adopts[0] != 7 {
		t.Errorf("slots %v, adopts %v", r.slots, r.adopts)
	}
	for _, bad := range []sandbox.Request{
		{Op: sandbox.OpCapSlot, Slice: "app.slice", Max: testMax, Swap: "0"},
		{Op: sandbox.OpCapSlot, Slice: "memcap-slot1_0a1b2c3d.slice/../app.slice", Max: testMax, Swap: "0"},
		{Op: sandbox.OpScope, Unit: "app-com.anthropic.Claude-1", Slice: testMemcapSlice, Max: testMax, Swap: "0"},
		{Op: sandbox.OpScope, Unit: testRunUnit, Slice: testMemcapSlice, Max: "lots", Swap: "0"},
		{Op: sandbox.OpScope, Unit: testRunUnit, Slice: testMemcapSlice, Max: testMax, Swap: ""},
		{Op: "exec", Slice: testMemcapSlice, Max: testMax, Swap: "0"},
	} {
		if err := h(7, bad); err == nil {
			t.Errorf("%+v: want a refusal", bad)
		}
	}
	if len(r.slots) != 1 || len(r.adopts) != 2 {
		t.Errorf("a refused request reached the host: slots %v, adopts %v", r.slots, r.adopts)
	}
}

func TestBrokeredSecretArgs(t *testing.T) {
	for _, ok := range [][]string{
		{compareOp, sopsA, sopsB},
		{copyOp, "--name=--config", sopsA, sopsB},
		{"rotate", "op://Shared/db/password", "--generate=true", "--json=true"},
	} {
		if err := brokeredSecretArgs(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range [][]string{
		nil,
		{"setup"},
		{"import", "op://Private/x/y", "op://Shared/x/y"},
		{copyOp, "a.sops.yaml#k", "--", "sh", "-c", "env"},
		{compareOp, "--as", agentTwo, "a", "b"},
		{compareOp, "--config=/tmp/other.yaml", "a", "b"},
	} {
		if err := brokeredSecretArgs(bad); err == nil {
			t.Errorf("%q: want a refusal", bad)
		}
	}
}

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
	h := brokered(brokeredCap(&recordingCapper{}), brokeredSecret(exe, "/proc"))
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
