package cmd

import (
	"strings"
	"testing"
)

// TestServingModesNoWidening: a constraint that allows a wider mode than the
// model runs is no request to switch up — the machine moved down for a
// reason and stays down until the constraint falls; the wider mode comes
// back with the release.
func TestServingModesNoWidening(t *testing.T) {
	s := newServingModesTest(t)
	s.setMode(servingModeSingle)
	s.agents("BusyOne", "BusyTwo")

	// node-b is free, single runs free of it, and the constraints allow
	// the full mode: still the claim of node-b switches nothing. The
	// model drains only to run free of a held node, never up.
	if out, err := s.claimNode("GpuJob"); err != nil {
		t.Fatalf("claim: %v\n%s", err, out)
	}
	if m := s.mode(); m != servingModeSingle {
		t.Fatalf("the mode is %q after the claim, want %q", m, servingModeSingle)
	}
	if evs := s.events(verbServeSwitch); len(evs) != 0 {
		t.Fatalf("a claim widened the model:\n%s", eventLines(evs))
	}
	if line := s.capacity(); !strings.Contains(line, "2 busy of ceiling 2: BusyOne, BusyTwo; serving mode single") {
		t.Fatalf("capacity while the constraint holds:\n%s", line)
	}
	// The release asks for the widest mode free of no node: full, its
	// ceiling with it.
	if out, err := s.releaseNode("GpuJob"); err != nil {
		t.Fatalf("release: %v\n%s", err, out)
	}
	if m := s.mode(); m != servingModeFull {
		t.Fatalf("the mode is %q after the release, want %q", m, servingModeFull)
	}
}
