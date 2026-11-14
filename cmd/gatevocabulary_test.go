package cmd

import (
	"testing"

	"github.com/giantswarm/beekeeper/internal/gate"
)

// TestGateVocabulary keeps the copy the gate has of the command's names equal to
// the command's own: it cannot import them.
//
// control: none: shares the vocabClaudeComm key constant with the new watch vocabulary
// test without changing the checked value, and package cmd does not build on the merge base.
func TestGateVocabulary(t *testing.T) {
	own := map[string]any{
		"ExitCentral":    ExitCentral,
		vocabClaudeComm:  claudeComm,
		"holdParkFlag":   holdParkFlag,
		"paramReason":    paramReason,
		"paramRepo":      paramRepo,
		"squashMergeCmd": squashMergeCmd,
	}
	got := gate.Vocabulary()
	if len(got) != len(own) {
		t.Fatalf("gate names %d constants, the test %d", len(got), len(own))
	}
	for name, want := range own {
		if got[name] != want {
			t.Errorf("gate %s = %v, the command's is %v", name, got[name], want)
		}
	}
}
