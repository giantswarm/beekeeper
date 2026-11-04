package examples_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/docs/examples"
	"github.com/giantswarm/beekeeper/internal/config"
)

func TestConfigPassesCheck(t *testing.T) {
	home := t.TempDir()
	// Empty local inputs suffice: Check only verifies that these files exist.
	for _, name := range []string{
		".kube/config",
		".local/state/beekeeper/kube/teleport.yaml",
		".local/state/desk/report-brief.md",
		".omp/agent/models.yml",
	} {
		path := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(home, "config.yaml")
	raw := strings.ReplaceAll(examples.Config, "~/", filepath.ToSlash(home)+"/")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	findings, err := config.Check(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("example config findings: %v", findings)
	}
}
