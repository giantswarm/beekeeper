package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const configHookVerb = "hook"
const configPreToolUse = "pretooluse"

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

	if stderr, err := execute(t, "log", "-n", "1"); err != nil || stderr != "" {
		t.Errorf("log = %v, stderr %q", err, stderr)
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
	if stderr, err := execute(t, "log", "-n", "1"); err != nil || stderr != "" {
		t.Errorf("after set: log = %v, %q", err, stderr)
	}
	if _, err := execute(t, "config", "set", "secret.store"); Code(err) != ExitUsage {
		t.Errorf("odd arguments = exit %d, want %d", Code(err), ExitUsage)
	}
}

// Config failure must precede opening either the default store or any
// state directory named by the real configuration.
func TestMissingEnvironmentConfigDoesNotOpenState(t *testing.T) {
	for _, args := range [][]string{{verbHold}, {verbHold, setOp, holdMerges, "--reason=config-test", "--as=config-test-actor"}, {configFlag, setOp, "stateDir", "unused-state"}, {"run", "--", "printf"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			t.Setenv("APPDATA", root)
			t.Setenv("XDG_CONFIG_HOME", root)
			t.Setenv("XDG_STATE_HOME", root)
			realState := filepath.Join(root, "real-state")
			configDir, err := os.UserConfigDir()
			if err != nil {
				t.Fatal(err)
			}
			defaultConfig := filepath.Join(configDir, "beekeeper", "config.yaml")
			if err := os.MkdirAll(filepath.Dir(defaultConfig), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(defaultConfig, []byte("stateDir: "+realState+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			missing := filepath.Join(root, "missing.yaml")
			t.Setenv("BEEKEEPER_CONFIG", missing)
			_, err = execute(t, args...)
			if err == nil || Code(err) == 0 || !strings.Contains(err.Error(), missing) {
				t.Fatalf("command = %v; want error naming %s", err, missing)
			}
			for _, dir := range []string{realState, filepath.Join(root, "beekeeper", "state.json"), filepath.Join(root, "beekeeper", "state.lock")} {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Errorf("state path %s touched: %v", dir, err)
				}
			}
		})
	}
}

// Both spellings of an unselected environment path still use the default
// configuration, including its stateDir, for reads and writes.
func TestDefaultEnvironmentConfig(t *testing.T) {
	for _, unset := range []bool{false, true} {
		t.Run(fmt.Sprint(unset), func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			t.Setenv("APPDATA", root)
			t.Setenv("XDG_CONFIG_HOME", root)
			t.Setenv("XDG_STATE_HOME", root)
			t.Setenv("BEEKEEPER_CONFIG", "")
			if unset {
				if err := os.Unsetenv("BEEKEEPER_CONFIG"); err != nil {
					t.Fatal(err)
				}
			}
			configDir, err := os.UserConfigDir()
			if err != nil {
				t.Fatal(err)
			}
			defaultConfig := filepath.Join(configDir, "beekeeper", "config.yaml")
			if err := os.MkdirAll(filepath.Dir(defaultConfig), 0o700); err != nil {
				t.Fatal(err)
			}
			stateDir := filepath.Join(root, "chosen-state")
			if err := os.WriteFile(defaultConfig, []byte("stateDir: "+stateDir+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{verbHold}, {verbHold, setOp, holdMerges, "--reason=config-test", "--as=config-test-actor"}} {
				if _, err = execute(t, args...); err != nil {
					t.Fatalf("%v: %v", args, err)
				}
			}
			if _, err := os.Stat(filepath.Join(stateDir, "state.json")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A hook config failure must preserve the harness decision contract: deny
// tools, and give neither session setup nor permission auto-approval.
func TestMissingEnvironmentConfigHooks(t *testing.T) {
	const bashEvent = `{"hook_event_name":"PreToolUse","session_id":"config-hook","cwd":"/","tool_name":"Bash","tool_input":{"command":"kubectl --context kube.production delete ns blocked"}}`
	const denied = "deny"
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, hook := range []struct {
		name  string
		args  []string
		input string
		deny  bool
	}{
		{claudeComm, []string{configPreToolUse}, bashEvent, true},
		{"codex", []string{configPreToolUse, "--format=codex"}, bashEvent, true},
		{"session-start", []string{"sessionstart"}, `{"hook_event_name":"SessionStart","session_id":"config-hook"}`, false},
		{"permission-request", []string{"permissionrequest"}, `{"hook_event_name":"PermissionRequest","session_id":"config-hook","tool_name":"Bash","tool_input":{"command":"kubectl --context kube.production delete ns blocked"}}`, false},
	} {
		t.Run(hook.name, func(t *testing.T) {
			root := t.TempDir()
			stateDir := filepath.Join(root, "state")
			configDir := filepath.Join(root, "config")
			prelude := filepath.Join(root, "prelude")
			t.Setenv("HOME", root)
			t.Setenv("APPDATA", configDir)
			t.Setenv("XDG_CONFIG_HOME", configDir)
			t.Setenv("XDG_STATE_HOME", stateDir)
			t.Setenv("CLAUDE_ENV_FILE", prelude)
			for _, broken := range []bool{false, true} {
				t.Run(fmt.Sprint(broken), func(t *testing.T) {
					cfg := filepath.Join(root, "missing.yaml")
					if broken {
						cfg = filepath.Join(root, "unparseable.yaml")
						if err := os.WriteFile(cfg, []byte("invalid: [yaml\n"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					call := exec.Command(self, append([]string{configHookVerb}, hook.args...)...) //nolint:gosec // this test binary, run as beekeeper
					call.Env = append(os.Environ(), "BEEKEEPER_TEST_MAIN=1", "BEEKEEPER_CONFIG="+cfg)
					call.Stdin = strings.NewReader(hook.input)
					var stdout, stderr bytes.Buffer
					call.Stdout, call.Stderr = &stdout, &stderr
					if err := call.Run(); err != nil {
						t.Fatalf("hook: %v; stdout %q, stderr %q", err, stdout.String(), stderr.String())
					}
					if stderr.Len() != 0 {
						t.Fatalf("hook stderr: %q", stderr.String())
					}
					if hook.deny {
						var out struct {
							HookSpecificOutput struct {
								Decision string `json:"permissionDecision"`
								Reason   string `json:"permissionDecisionReason"`
							} `json:"hookSpecificOutput"`
						}
						if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
							t.Fatal(err)
						}
						if out.HookSpecificOutput.Decision != denied || !strings.Contains(out.HookSpecificOutput.Reason, cfg) {
							t.Fatalf("want deny naming %s, got %s", cfg, stdout.String())
						}
					} else if stdout.Len() != 0 {
						t.Fatalf("want no hook response, got %q", stdout.String())
					}
					for _, path := range []string{stateDir, configDir, filepath.Join(root, "Library"), prelude} {
						if _, err := os.Stat(path); !os.IsNotExist(err) {
							t.Errorf("hook touched %s: %v", path, err)
						}
					}
				})
			}
		})
	}
}
