package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
	// The host's CPU budget reaches the slot's cap; the request carries none.
	h := brokeredCap(r, platform.Cap{CPUQuota: "1200%", CPUWeight: 50})
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
	want := platform.Cap{Max: testMax, Swap: "0", Slice: "memcap-slot1_0a1b2c3d.slice", CPUQuota: "1200%", CPUWeight: 50}
	if len(r.slots) != 1 || r.slots[0] != want || len(r.adopts) != 2 || r.adopts[0] != 7 {
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
		if err := brokeredSecretArgs(ok, true); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	// a requester outside the sandbox runs its consumer on the host anyway;
	// copy's own consumer list holds it
	consumer := strings.Fields(copyOp + " op://Shared/x/y -- gh secret set T")
	if err := brokeredSecretArgs(consumer, false); err != nil {
		t.Errorf("%q outside the sandbox: %v", consumer, err)
	}
	if err := brokeredSecretArgs(consumer, true); err == nil {
		t.Errorf("%q in the sandbox: want a refusal", consumer)
	}
	for _, bad := range [][]string{
		nil,
		{"setup"},
		{"import", "op://Private/x/y", "op://Shared/x/y"},
		{copyOp, "a.sops.yaml#k", "--", "sh", "-c", "env"},
		{compareOp, "--as", agentTwo, "a", "b"},
		{compareOp, "--config=/tmp/other.yaml", "a", "b"},
	} {
		if err := brokeredSecretArgs(bad, true); err == nil {
			t.Errorf("%q: want a refusal", bad)
		}
	}
}

func TestSandboxInstallSteps(t *testing.T) {
	dir, state := filepath.Join(t.TempDir(), "claude-code"), t.TempDir()
	policy := []byte("{\"sandbox\":{}}\n")
	steps, err := sandboxInstallSteps(dir, state, policy)
	if err != nil {
		t.Fatal(err)
	}
	settings, dropIns := filepath.Join(dir, "managed-settings.json"), filepath.Join(dir, "managed-settings.d")
	want := []string{
		"sudo install -d -m 0755 " + dir,
		"sudo install -m 0644 -o root " + filepath.Join(state, "managed-settings.json") + " " + settings,
		"sudo install -d -m 0755 " + dropIns,
		"sudo install -m 0644 -o root " + filepath.Join(state, sandbox.DropIn) + " " + filepath.Join(dropIns, sandbox.DropIn),
	}
	if !slices.Equal(steps, want) {
		t.Fatalf("a fresh machine's steps = %q, want %q", steps, want)
	}
	if b, _ := os.ReadFile(filepath.Join(state, "managed-settings.json")); string(b) != emptyManagedSettings { //nolint:gosec // the test's own temporary directory
		t.Errorf("staged managed settings = %q, want {}", b)
	}
	// the root steps run
	for _, f := range []struct {
		path string
		b    []byte
	}{{settings, []byte(emptyManagedSettings)}, {filepath.Join(dropIns, sandbox.DropIn), policy}} {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.path, f.b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if steps, err := sandboxInstallSteps(dir, state, policy); err != nil || len(steps) != 0 {
		t.Errorf("an installed policy's steps = %q, %v, want none", steps, err)
	}
	steps, err = sandboxInstallSteps(dir, state, []byte("{\"sandbox\":{\"enabled\":true}}\n"))
	if err != nil || len(steps) != 1 || !strings.HasSuffix(steps[0], sandbox.DropIn) {
		t.Errorf("a changed policy's steps = %q, %v, want the drop-in only", steps, err)
	}
}

func TestBrokeredGateArgv(t *testing.T) {
	for in, want := range map[string]string{
		"devctl pr merge giantswarm/beekeeper 7":                  "gate --wait 2m0s -- devctl pr merge giantswarm/beekeeper 7",
		"/home/u/.go/bin/devctl pr wait giantswarm/beekeeper 7":   "gate --wait 2m0s -- devctl pr wait giantswarm/beekeeper 7",
		"devctl release promote giantswarm/beekeeper":             "gate --wait 2m0s -- devctl release promote giantswarm/beekeeper",
		"devctl rollout wait gazelle giantswarm/backstage --pr 3": "gate --wait 2m0s -- devctl rollout wait gazelle giantswarm/backstage --pr 3",
	} {
		got, err := brokeredGateArgv(sandbox.Request{Op: sandbox.OpGate, Args: strings.Fields(in), Wait: "2m"}, true)
		if err != nil || strings.Join(got, " ") != want {
			t.Errorf("%s: %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []sandbox.Request{
		{Args: strings.Fields("sh -c id"), Wait: "2m"},
		{Args: strings.Fields("/tmp/x/devctl-evil pr merge giantswarm/beekeeper 7"), Wait: "2m"},
		{Args: strings.Fields("devctl repo create giantswarm/x"), Wait: "2m"},
		{Args: strings.Fields("devctl pr merge giantswarm/beekeeper 7"), Wait: "forever"},
		{Args: strings.Fields("devctl pr merge giantswarm/beekeeper 7"), Wait: "24h"},
		{},
	} {
		if got, err := brokeredGateArgv(bad, true); err == nil {
			t.Errorf("%+v: %q, want a refusal", bad, got)
		}
	}
}

func TestDevctlPath(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	if got := devctlPath("/home/u/.go/bin/devctl"); len(got) != 1 || got[0] != "PATH=/home/u/.go/bin:/usr/bin" {
		t.Errorf("devctlPath = %q", got)
	}
	if got := devctlPath("devctl"); got != nil {
		t.Errorf("devctl on PATH: %q, want the broker's PATH", got)
	}
}
