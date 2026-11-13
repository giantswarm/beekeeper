package cmd

import (
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestFindingDescriptionsSurviveBothReportPaths(t *testing.T) {
	const body = "example/app: Queue stalls\n\n## Problem\nThe queue stalls.\n\n## Acceptance criteria\nPreserve queued work."
	if got := normalizeFinding(body); got != body {
		t.Fatalf("flattened problem: %q", got)
	}
	found, err := problemsFound([]string{body}, true)
	if err != nil || len(found) != 1 || found[0] != body {
		t.Fatalf("flattened idle problem: %q, %v", found, err)
	}
	ag := state.Agent{Task: "Investigate queue"}
	got := freshWorkerProblems(&ag, found)
	if len(got) != 1 || got[0] != body {
		t.Fatalf("flattened receipt: %q", got)
	}
	if got := freshWorkerProblems(&ag, found); len(got) != 0 {
		t.Fatalf("repeated report: %q", got)
	}
}
