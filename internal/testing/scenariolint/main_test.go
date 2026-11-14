package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/giantswarm/beekeeper/internal/testing/harness"
)

const scenarios = "../scenarios"

// TestCatalogueIsCurrent fails when a scenario changed without regenerating CATALOGUE.md.
func TestCatalogueIsCurrent(t *testing.T) {
	t.Parallel()
	all, err := harness.LoadDir(scenarios)
	if err != nil {
		t.Fatal(err)
	}
	have, err := os.ReadFile(filepath.Join(scenarios, "CATALOGUE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(have) != harness.Catalogue(all) {
		t.Fatalf("CATALOGUE.md is stale: run go run ./internal/testing/scenariolint internal/testing/scenarios")
	}
}
