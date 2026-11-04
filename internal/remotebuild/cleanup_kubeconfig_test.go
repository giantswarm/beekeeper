package remotebuild

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	sweepContext     = "sweep-builders"
	sweepContextFlag = "--context"
	sweepNamespace   = "sweep-workers"
)

func TestSweepKubeconfigAcrossDirectories(t *testing.T) {
	for _, conflicting := range []bool{false, true} {
		label := "missing"
		if conflicting {
			label = "conflicting"
		}
		t.Run(label, func(t *testing.T) {
			origin, desk, state := t.TempDir(), t.TempDir(), t.TempDir()
			t.Chdir(origin)
			origin, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			kubeconfig := filepath.Join(origin, "build.yaml")
			if err := os.WriteFile(kubeconfig, []byte("original connection"), 0o600); err != nil {
				t.Fatal(err)
			}
			if conflicting {
				if err := os.WriteFile(filepath.Join(desk, "build.yaml"), []byte("wrong connection"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(filepath.Join(state, "remote-build"), 0o700); err != nil {
				t.Fatal(err)
			}
			host, err := os.Hostname()
			if err != nil {
				t.Fatal(err)
			}
			name := builderName(host, origin)
			runner := Runner{StateDir: state, Config: Config{Kubeconfig: "build.yaml", Context: sweepContext, Namespace: sweepNamespace}}
			if err := runner.remember(origin, host, name); err != nil {
				t.Fatal(err)
			}
			rec := builderRecord{origin, host, kubeconfig, sweepContext, sweepNamespace}
			path := filepath.Join(state, "remote-build", rec.file(name))
			raw, err := os.ReadFile(path) //nolint:gosec // owned test fixture
			if err != nil {
				t.Fatal(err)
			}
			var saved builderRecord
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			if saved != rec {
				t.Fatalf("saved record = %+v, want %+v", saved, rec)
			}
			t.Chdir(desk)
			runner.Config = Config{Kubeconfig: "build.yaml", Context: "different", Namespace: "different"}
			calls := 0
			runner.Command = func(_ context.Context, _ io.Reader, _ io.Writer, args ...string) error {
				calls++
				want := []string{"--kubeconfig", kubeconfig, sweepContextFlag, sweepContext, "--namespace", sweepNamespace}
				if !slices.Equal(args[:6], want) {
					t.Fatalf("connection = %v, want %v", args[:6], want)
				}
				contents, err := os.ReadFile(args[1]) //nolint:gosec // owned test fixture
				if err != nil || string(contents) != "original connection" {
					t.Fatalf("connection contents = %q, error = %v", contents, err)
				}
				return nil
			}
			if err := runner.Sweep(context.Background(), []string{origin}, false); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("cluster calls = %d, want 1", calls)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("record remains: %v", err)
			}
		})
	}
}

func TestSweepRetainsLegacyRelativeKubeconfig(t *testing.T) {
	state, root := t.TempDir(), t.TempDir()
	t.Chdir(t.TempDir())
	if err := os.WriteFile("build.yaml", []byte("unrelated connection"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(state, "remote-build")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	name := builderName(host, root)
	rec := builderRecord{root, host, "build.yaml", sweepContext, sweepNamespace}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, rec.file(name))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := Runner{StateDir: state, Command: func(context.Context, io.Reader, io.Writer, ...string) error {
		t.Fatal("legacy relative connection must never reach kubectl")
		return nil
	}}
	for _, dry := range []bool{true, false, false} {
		err := runner.Sweep(context.Background(), []string{root}, dry)
		if err == nil || !strings.Contains(err.Error(), "original absolute kubeconfig path") {
			t.Fatalf("expected actionable legacy error, got %v", err)
		}
		kept, err := os.ReadFile(path) //nolint:gosec // owned test fixture
		if err != nil || string(kept) != string(raw) {
			t.Fatalf("legacy record changed: %v", err)
		}
	}
}

// The default kubeconfig has no path: its record must not gain one.
func TestRememberKeepsTheDefaultKubeconfigEmpty(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "remote-build"), 0o700); err != nil {
		t.Fatal(err)
	}
	const host, name = "fixture-host", "fixture-builder"
	runner := Runner{StateDir: state, Config: Config{Context: sweepContext, Namespace: sweepNamespace}}
	if err := runner.remember(root, host, name); err != nil {
		t.Fatal(err)
	}
	rec := builderRecord{root, host, "", sweepContext, sweepNamespace}
	raw, err := os.ReadFile(filepath.Join(state, "remote-build", rec.file(name))) //nolint:gosec // owned test fixture
	if err != nil {
		t.Fatal(err)
	}
	var saved builderRecord
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved != rec {
		t.Fatalf("saved record = %+v, want %+v", saved, rec)
	}
}
