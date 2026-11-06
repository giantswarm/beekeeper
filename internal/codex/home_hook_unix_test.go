//go:build unix

package codex

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestHooksJSONExecutesLiteralBinaryPath(t *testing.T) {
	for _, name := range []string{"worker tools", "worker's tools", "worker$missing; tools"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(dir, "beekeeper")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil { //nolint:gosec // executable offline hook fixture in a test-owned directory
				t.Fatal(err)
			}
			raw, err := HooksJSON(bin)
			if err != nil {
				t.Fatal(err)
			}
			var hooks hooksFile
			if err := json.Unmarshal(raw, &hooks); err != nil {
				t.Fatal(err)
			}
			command := hooks.Hooks["PreToolUse"][0].Hooks[0].Command
			out, err := exec.Command("sh", "-c", command).CombinedOutput() //nolint:gosec // run only the generated command for this test's synthetic executable
			if err != nil || string(out) != "hook\npretooluse\n--format\ncodex\n" {
				t.Fatalf("generated hook did not invoke the literal binary: %v: %s", err, out)
			}
		})
	}
}
