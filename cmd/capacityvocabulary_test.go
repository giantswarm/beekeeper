package cmd

import (
	"testing"

	"github.com/giantswarm/beekeeper/internal/capacity"
)

// TestCapacityVocabulary keeps the copy capacity has of the command's names equal to
// the command's own: it cannot import them.
func TestCapacityVocabulary(t *testing.T) {
	own := map[string]any{
		"claudeComm": claudeComm,
		"noProblems": noProblems,
		"workerRole": workerRole,
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
