package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
)

func TestOmpModelsFileHomePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	const models = "fixture-models.yml"
	if err := os.WriteFile(filepath.Join(home, models), []byte("providers: {fixture: {apiKey: FIXTURE_API_KEY}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("omp: {modelsFile: ~/"+models+"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if findings, err := config.Check(p); err != nil || len(findings) != 0 {
		t.Fatalf("Check = %v, %v", findings, err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	// This is the offline read used by cmd.ompCredentials, before any secret
	// resolution or process start. The fixture contains a variable name only.
	if name, err := omp.ProviderKey(c.Omp.ModelsFile, "fixture"); err != nil || name != "FIXTURE_API_KEY" {
		t.Fatalf("configured models file did not reach credential lookup: name %q, error %v", name, err)
	}
}

func TestOmpSessionsDirHomePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, "sessions", "project")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(dir, "fixture.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"session","id":"fixture","cwd":"/fixture"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("omp: {sessionsDir: ~/sessions}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if ended := omp.Ended(c.Omp.SessionsDir, nil, time.Time{}); len(ended) != 1 || ended[0].ID != "fixture" || ended[0].Transcript != transcript {
		t.Fatalf("configured sessions directory did not reach session discovery: %v", ended)
	}
}

func TestCheckedInputPathsUseHome(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		path func(*config.Config) string
	}{
		{"remote kubeconfig", "run: {remote: {enabled: true, kubeconfig: ~/input, context: fixture, namespace: builders, images: {go: 'builder:fixture'}}}", func(c *config.Config) string { return c.Run.Remote.Kubeconfig }},
		{"gateway token file", "serve: {gateway: {tokenFile: ~/input}}", func(c *config.Config) string { return c.Serve.Gateway.TokenFile }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			if err := os.WriteFile(filepath.Join(home, "input"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if findings, err := config.Check(p); err != nil || len(findings) != 0 {
				t.Fatalf("Check = %v, %v", findings, err)
			}
			c, err := config.Load(p)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(tc.path(c)); err != nil {
				t.Fatalf("checked input is unavailable at the loaded path: %v", err)
			}
		})
	}
}
