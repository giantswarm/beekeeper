//go:build (linux && !nosystemd) || darwin

package cmd

import (
	"encoding/json"
	"io"
	"path/filepath"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/install"
)

func TestSupervisorLoopInstallerCapturesToolPATH(t *testing.T) {
	path := filepath.Join(t.TempDir(), "interactive-tools")
	t.Setenv("PATH", path)
	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: cfg, out: io.Discard}
	var e install.Env
	if err := a.installEnv(&e, "/opt/bin/beekeeper"); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e.Spec)
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if spec["ToolPath"] != path {
		t.Fatal("installer did not retain its tool PATH for the loop service")
	}
}
