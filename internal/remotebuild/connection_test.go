//nolint:goconst // Repeated fixture and protocol literals make the failure scenarios readable.
package remotebuild

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSweepPinsRelativeKubeconfig(t *testing.T) {
	origin, elsewhere, root, state := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	for dir, data := range map[string]string{origin: "original synthetic cluster", elsewhere: "different synthetic cluster"} {
		if err := os.WriteFile(filepath.Join(dir, "connection.yaml"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(state, "remote-build"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(origin)
	var selected string
	runner := Runner{StateDir: state, Config: Config{Kubeconfig: "connection.yaml", Context: "connection-fixture", Namespace: "connection-fixture"},
		Command: func(_ context.Context, _ io.Reader, _ io.Writer, args ...string) error {
			// Read only the synthetic connection file the transport would select.
			data, err := os.ReadFile(args[1]) //nolint:gosec // paths are owned fixture connections
			selected = string(data)
			return err
		}}
	if err := runner.remember(root, host, builderName(host, root)); err != nil {
		t.Fatal(err)
	}
	t.Chdir(elsewhere)
	if err := runner.Sweep(context.Background(), []string{root}, false); err != nil {
		t.Fatal(err)
	}
	if selected != "original synthetic cluster" {
		t.Fatalf("retirement selected %q after cwd changed", selected)
	}
}

func TestSweepRetainsLegacyRelativeConnection(t *testing.T) {
	state, root := t.TempDir(), t.TempDir()
	dir := filepath.Join(state, "remote-build")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	name := builderName(host, root)
	rec := builderRecord{Root: root, Host: host, Kubeconfig: "connection.yaml", Context: "connection-fixture", Namespace: "connection-fixture"}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, rec.file(name))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	runner := Runner{StateDir: state, Command: func(context.Context, io.Reader, io.Writer, ...string) error {
		calls++
		return nil
	}}
	err = runner.Sweep(context.Background(), []string{root}, false)
	if err == nil || !strings.Contains(err.Error(), "relative kubeconfig") {
		t.Errorf("unsafe legacy connection was not reported: %v", err)
	}
	if calls != 0 {
		t.Errorf("retirement guessed the legacy connection: %d transport calls", calls)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("legacy ownership record must remain for repair: %v", err)
	}
}
