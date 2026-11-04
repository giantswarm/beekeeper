package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
)

func TestOmpModelsFileHomeRelative(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BEEKEEPER_EXECUTION_HOME", "")
	modelsPath := filepath.Join(home, "fixture-models.yml")
	if err := os.WriteFile(modelsPath, []byte("providers: {fixture: {apiKey: FIXTURE_API_KEY}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("omp: {modelsFile: ~/fixture-models.yml}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	findings, err := config.Check(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("Check findings = %v", findings)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Omp.ModelsFile != modelsPath {
		t.Errorf("modelsFile = %q, want %q", c.Omp.ModelsFile, modelsPath)
	}
	key, err := omp.ProviderKey(c.Omp.ModelsFile, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if key != "FIXTURE_API_KEY" {
		t.Errorf("ProviderKey = %q, want FIXTURE_API_KEY", key)
	}
}
