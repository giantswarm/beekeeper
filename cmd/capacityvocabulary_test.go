package cmd

import (
	"testing"

	"github.com/giantswarm/beekeeper/internal/capacity"
)

const vocabClaudeComm = "claudeComm"

// TestCapacityVocabulary keeps the copy capacity has of the command's names equal to
// the command's own: it cannot import them.
//
// control: none: shares the vocabClaudeComm key constant with the new watch vocabulary
// test without changing the checked value, and package cmd does not build on the merge base.
func TestCapacityVocabulary(t *testing.T) {
	own := map[string]any{
		vocabClaudeComm: claudeComm,
		"noProblems":    noProblems,
		"workerRole":    workerRole,
	}
	got := capacity.Vocabulary()
	if len(got) != len(own) {
		t.Fatalf("capacity names %d constants, the test %d", len(got), len(own))
	}
	for name, want := range own {
		if got[name] != want {
			t.Errorf("capacity %s = %v, the command's is %v", name, got[name], want)
		}
	}
}
