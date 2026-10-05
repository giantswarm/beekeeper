package cmd

import (
	"os/exec"
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
