package cmd

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

const incompleteConfig = "secret: {ageIdentities: [{recipient: age1x, ref: store://keys/age}]}\n"

func execute(t *testing.T, args ...string) (stderr string, err error) {
	t.Helper()
	var errOut bytes.Buffer
	root := New()
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(&errOut)
	err = root.Execute()
	return errOut.String(), err
}

// A store:// reference without secret.store leaves every command working
// and warns once on beekeeper secret.
func TestIncompleteConfigLoads(t *testing.T) {
	cfg := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(cfg, []byte(incompleteConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEEKEEPER_CONFIG", cfg)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if stderr, err := execute(t, "lease", "list"); err != nil || stderr != "" {
		t.Errorf("lease list = %v, stderr %q", err, stderr)
	}
	stderr, _ := execute(t, "secret", "compare", "op://V/i/a", "op://V/i/b")
	if n := strings.Count(stderr, "warning: secret.ageIdentities[0]"); n != 1 || !strings.Contains(stderr, "takes secret.store.read") {
		t.Errorf("secret stderr = %q, want one warning", stderr)
	}
}

// config set writes cross-referenced keys in one call and refuses a result
// that would not load, the file unchanged; it runs on a broken file too.
func TestConfigSet(t *testing.T) {
	cfg := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(cfg, []byte("grantTTL: soon\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEEKEEPER_CONFIG", cfg)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if _, err := execute(t, "config", "set", "secret.store", "{read: [r]}"); Code(err) != ExitRefused {
		t.Errorf("set on a broken file keeping it broken = exit %d (%v), want %d", Code(err), err, ExitRefused)
	}
	if _, err := execute(t, "config", "set", "grantTTL", "10m",
		"secret.ageIdentities", `[{recipient: age1x, ref: "store://keys/age"}]`,
		"secret.store", "{read: [r]}"); err != nil {
		t.Fatalf("set = %v", err)
	}
	if stderr, err := execute(t, "lease", "list"); err != nil || stderr != "" {
		t.Errorf("after set: lease list = %v, %q", err, stderr)
	}
	if _, err := execute(t, "config", "set", "secret.store"); Code(err) != ExitUsage {
		t.Errorf("odd arguments = exit %d, want %d", Code(err), ExitUsage)
	}
}
