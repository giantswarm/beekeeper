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
	// Supply only synthetic local inputs. Check stats these files without
	// reading them, resolving credentials or contacting the example services.
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
	// Replace the example's home paths before checking, including the inputs
	// whose existence Check resolves independently of the loader's defaults.
	raw := strings.ReplaceAll(examples.Config, "~/", filepath.ToSlash(home)+"/")
	path := filepath.Join(home, "example.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	findings, err := config.Check(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("example fails config check:\n%s", strings.Join(findings, "\n"))
	}
}
