//go:build (linux && !nosystemd) || darwin

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/install"
	"github.com/giantswarm/beekeeper/internal/platform"
)

// Keep the native renderer and service commands, but never call the host's
// service manager or write outside this test's home.
type loopServiceSetup struct {
	platform.Setup
	started bool
}

func (*loopServiceSetup) Available() bool                        { return true }
func (s *loopServiceSetup) Started(context.Context, string) bool { return s.started }

func TestSupervisorLoopServiceLifecycle(t *testing.T) {
	const (
		keepCustom = "keep-custom"
		optOut     = "opt-out"
	)
	for _, transition := range []string{optOut, "dry-run", "install", "repeat", "update-binary", "start-stopped", "uninstall", keepCustom} {
		t.Run(transition, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "loop.yaml")
			if err := os.WriteFile(path, []byte("supervisor: {loop: "+strconv.FormatBool(transition != optOut)+"}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			a := &app{cfg: cfg, cfgPath: path, out: &out}
			var e install.Env
			if err := a.installEnv(&e, "/opt/bin/beekeeper"); err != nil {
				t.Fatal(err)
			}
			e.Spec.Home, e.Spec.ConfigDir = home, filepath.Join(home, "config")
			e.Spec.RAMMiB, e.Spec.TeleportEvery = 0, 0
			e.Settings, e.StateDir = filepath.Join(home, "settings.json"), filepath.Join(home, "state")
			e.AgyHooks, e.AgySkills, e.CodexHooks, e.CodexSkills = "", "", "", ""
			setup := &loopServiceSetup{Setup: e.Setup}
			e.Setup = setup
			var commands []string
			e.Run = func(_ context.Context, argv []string) error {
				commands = append(commands, strings.Join(argv, " "))
				return nil
			}
			files, _ := setup.Files(e.Spec)
			var unit platform.File
			for _, f := range files {
				if strings.Contains(filepath.Base(f.Path), "supervisor-loop") {
					unit = f
				}
			}
			if unit.Path == "" || !unit.Service {
				t.Fatalf("supervisor.loop: %v produced no loop service: the loop is no option", transition != optOut)
			}
			if transition == optOut {
				return
			}
			if !bytes.Contains(unit.Content, []byte(path)) || !bytes.Contains(unit.Content, []byte(e.Exe)) {
				t.Fatalf("unit lost explicit config or binary: %s", unit.Content)
			}
			if transition == keepCustom {
				if err := os.MkdirAll(filepath.Dir(unit.Path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(unit.Path, []byte("custom unit\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			e.DryRun = transition == "dry-run"
			if err := install.Install(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			if e.DryRun {
				if _, err := os.Stat(unit.Path); !os.IsNotExist(err) || len(commands) != 0 {
					t.Fatal("dry-run wrote or started loop")
				}
				if !strings.Contains(out.String(), filepath.Base(unit.Path)) {
					t.Fatal("dry-run did not name loop")
				}
				return
			}
			if transition == keepCustom {
				raw, _ := os.ReadFile(unit.Path)
				if string(raw) != "custom unit\n" {
					t.Fatal("overwrote custom unit")
				}
				for _, c := range commands {
					if strings.Contains(c, filepath.Base(unit.Path)) {
						t.Fatal("started custom unit")
					}
				}
				return
			}
			raw, err := os.ReadFile(filepath.Join(e.StateDir, "install.json"))
			if err != nil {
				t.Fatal(err)
			}
			var manifest install.Manifest
			if err := json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(manifest.Services, unit.Path) || manifest.Files[unit.Path] == "" {
				t.Fatal("manifest did not record loop for uninstall")
			}
			if !strings.Contains(strings.Join(commands, "\n"), strings.Join(setup.Start(unit.Path), " ")) {
				t.Fatalf("loop was not enabled and started: %v", commands)
			}
			commands = nil
			setup.started = true
			switch transition {
			case "repeat":
				if err := install.Install(context.Background(), e); err != nil {
					t.Fatal(err)
				}
				if len(commands) != 0 {
					t.Fatalf("repeat changed services: %v", commands)
				}
			case "update-binary":
				e.Exe, e.Spec.Exe = "/opt/new/beekeeper", "/opt/new/beekeeper"
				if err := install.Install(context.Background(), e); err != nil {
					t.Fatal(err)
				}
				updated, _ := os.ReadFile(unit.Path)
				if !bytes.Contains(updated, []byte(e.Exe)) {
					t.Fatal("unit still runs old binary")
				}
				for _, argv := range setup.Restart(unit.Path) {
					if !slices.Contains(commands, strings.Join(argv, " ")) {
						t.Fatalf("updated loop not restarted: %v", commands)
					}
				}
			case "start-stopped":
				setup.started = false
				if err := install.Install(context.Background(), e); err != nil {
					t.Fatal(err)
				}
				if !slices.Contains(commands, strings.Join(setup.Start(unit.Path), " ")) {
					t.Fatalf("stopped loop not started: %v", commands)
				}
			case "uninstall":
				if err := install.Uninstall(context.Background(), e, false); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(unit.Path); !os.IsNotExist(err) {
					t.Fatal("uninstall left loop unit")
				}
				if !slices.Contains(commands, strings.Join(setup.Stop(unit.Path), " ")) {
					t.Fatalf("loop not stopped and disabled: %v", commands)
				}
			}
		})
	}
}
