//go:build linux

package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/sandbox"
)

const (
	scratchSuccess     = "success"
	scratchUnchanged   = "unchanged"
	scratchVaultSocket = "vault-socket"
)

func TestScratchSecretBrokerLifecycle(t *testing.T) {
	for _, outcome := range []string{scratchSuccess, "failure", "interrupt", scratchVaultSocket} {
		t.Run(outcome, func(t *testing.T) {
			dir := t.TempDir()
			installed := filepath.Join(dir, "installed")
			runtimeDir := filepath.Join(dir, "runtime")
			for _, path := range []string{installed, runtimeDir} {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "sentinel"), []byte(scratchUnchanged), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			producer := filepath.Join(dir, "read-entry")
			// A local fallback would fail this check. The value never reaches output.
			if err := os.WriteFile(producer, []byte("#!/bin/sh\n[ \"$BEEKEEPER_SANDBOX_BROKERED\" = 1 ] || exit 19\nprintf 'scratch-test-value\\n'\n"), 0o700); err != nil { //nolint:gosec // the test's own secret producer
				t.Fatal(err)
			}
			cfg := map[string]any{scratchStateKey: installed, "leaseDir": installed, ghSecret: map[string]any{"store": map[string]any{"read": []string{producer}}}}
			if outcome == scratchVaultSocket {
				var secret map[string]any
				if err := yaml.Unmarshal(fmt.Appendf(nil, "{session: true, fileRoots: [%q], signinCommand: [\"false\"]}", dir), &secret); err != nil {
					t.Fatal(err)
				}
				cfg[ghSecret] = secret
				if err := os.WriteFile(filepath.Join(dir, "input.json"), []byte(`{"value":"scratch-test-value"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			data, err := yaml.Marshal(&cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			record := filepath.Join(dir, "record")
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			script := `set -eu
printf '%s\n%s\n' "$BEEKEEPER_CONFIG" "$XDG_RUNTIME_DIR" > "$1"
cat /proc/$PPID/task/$PPID/children >> "$1"
if "$2" --config "$4/config.yaml" secret fingerprint store://test#value; then
  exit 18
else
  test "$?" = 3
fi
if [ "$3" = vault-socket ]; then
  test -S "$XDG_RUNTIME_DIR/beekeeper/vault.sock"
  "$2" secret fingerprint "file://$4/input.json#value"
else
  "$2" secret fingerprint store://test#value
fi
printf '\nready\n' >> "$1"
case "$3" in
failure) exit 7;;
interrupt) exec sleep 60;;
esac
`
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			call := exec.CommandContext(ctx, exe, "sandbox", "scratch", "--", "sh", "-c", script, "sh", record, exe, outcome, dir) //nolint:gosec // the test's own binary and script
			call.Env = append(os.Environ(), "BEEKEEPER_TEST_MAIN=1", "BEEKEEPER_CONFIG="+path, "XDG_RUNTIME_DIR="+runtimeDir, sandbox.Env+"=", sandbox.Brokered+"=", scratchBrokerEnv+"=")
			var output bytes.Buffer
			call.Stdout, call.Stderr = &output, &output
			if err := call.Start(); err != nil {
				t.Fatal(err)
			}
			if outcome == "interrupt" {
				deadline := time.Now().Add(15 * time.Second)
				for {
					data, _ := os.ReadFile(record) //nolint:gosec // under the test's temporary directory
					if strings.Contains(string(data), "ready") {
						break
					}
					if time.Now().After(deadline) {
						_ = call.Process.Kill()
						_ = call.Wait()
						t.Fatalf("command never became ready: %s", output.String())
					}
					time.Sleep(20 * time.Millisecond)
				}
				if err := call.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			err = call.Wait()
			if (outcome == scratchSuccess || outcome == scratchVaultSocket) && err != nil {
				t.Fatalf("scratch: %v\n%s", err, output.String())
			}
			if outcome != scratchSuccess && outcome != scratchVaultSocket && err == nil {
				t.Fatal("expected failure")
			}
			if outcome == "failure" {
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 7 {
					t.Fatalf("exit: %v\n%s", err, output.String())
				}
			}
			if strings.Contains(output.String(), "scratch-test-value") {
				t.Fatal("value leaked")
			}
			data, err = os.ReadFile(record) //nolint:gosec // under the test's temporary directory
			if err != nil {
				t.Fatalf("record: %v\n%s", err, output.String())
			}
			lines := strings.Split(string(data), "\n")
			if !strings.Contains(string(data), "ready") {
				t.Fatalf("secret call did not complete: %s", output.String())
			}
			for _, path := range lines[:2] {
				if _, err := os.Stat(path); !os.IsNotExist(err) { //nolint:gosec // paths the scratch run recorded in this test
					t.Errorf("scratch path still exists: %s (%v)", path, err)
				}
			}
			for _, pidText := range strings.Fields(lines[2]) {
				pid, err := strconv.Atoi(pidText)
				if err != nil {
					t.Fatal(err)
				}
				if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
					t.Errorf("child %d still exists: %v", pid, err)
				}
			}
			for _, path := range []string{installed, runtimeDir} {
				entries, err := os.ReadDir(path)
				if err != nil || len(entries) != 1 {
					t.Fatalf("installed directory changed: %v, %v", entries, err)
				}
				data, err := os.ReadFile(filepath.Join(path, "sentinel")) //nolint:gosec // under the test's temporary directory
				if err != nil || string(data) != scratchUnchanged {
					t.Fatal(fmt.Sprint("installed sentinel changed: ", err))
				}
			}
		})
	}
}
