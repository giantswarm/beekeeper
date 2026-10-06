package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/giantswarm/beekeeper/internal/sandbox"
)

// unheldApp is an app in an environment that carries the sandbox policy's
// egress variables; what its load changes is restored after the test.
func unheldApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("stateDir: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(dir, "run")
	for _, k := range sandbox.Unset(sandbox.EgressDir(run)) {
		t.Setenv(k, os.Getenv(k))
	}
	t.Setenv("XDG_RUNTIME_DIR", run)
	t.Setenv(sandbox.Env, "1")
	t.Setenv(sandbox.Brokered, "")
	t.Setenv("GH_CONFIG_DIR", filepath.Join(sandbox.EgressDir(run), sandbox.EgressGH))
	return &app{cfgPath: cfg}
}

// Where the sandbox runtime does not hold beekeeper, its configuration
// load drops the policy's variables: its gh and git calls use the host's
// configuration, and inSandbox is false.
func TestLoadConfigUnconfinesAnUnheldSession(t *testing.T) {
	a := unheldApp(t)
	t.Setenv(sandbox.Runtime, "")
	if err := a.loadConfig(); err != nil {
		t.Fatal(err)
	}
	if inSandbox() {
		t.Error("inSandbox() after an unheld load")
	}
	if v, ok := os.LookupEnv("GH_CONFIG_DIR"); ok {
		t.Errorf("GH_CONFIG_DIR = %q", v)
	}
}

// A sandboxed command and a hook (which runs on the host for a sandboxed
// session too) keep the policy's environment.
func TestLoadConfigKeepsAHeldOrHookEnvironment(t *testing.T) {
	for name, held := range map[string]bool{"held": true, "hook": false} {
		t.Run(name, func(t *testing.T) {
			a := unheldApp(t)
			want := os.Getenv("GH_CONFIG_DIR")
			t.Setenv(sandbox.Runtime, "")
			if held {
				t.Setenv(sandbox.Runtime, "1")
			} else {
				a.hook = true
			}
			if err := a.loadConfig(); err != nil {
				t.Fatal(err)
			}
			if os.Getenv(sandbox.Env) != "1" || os.Getenv("GH_CONFIG_DIR") != want {
				t.Errorf("%s=%q GH_CONFIG_DIR=%q, want the policy's", sandbox.Env, os.Getenv(sandbox.Env), os.Getenv("GH_CONFIG_DIR"))
			}
		})
	}
}
