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
	for _, name := range []string{"worker tools", "worker's tools", "worker$missing; tools", `worker" tools`, "worker tools "} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(dir, "beekeeper ")
			// Each invocation prints a marker and every argument on its own line.
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' invoked \"$@\"\n"), 0o700); err != nil {
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
			cmd := exec.Command("sh", "-c", command)
			cmd.Env = []string{"PATH=/usr/bin:/bin"}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("hook execution failed: %v\n%s", err, out)
			}
			if got, want := string(out), "invoked\nhook\npretooluse\n--format\ncodex\n"; got != want {
				t.Fatalf("hook output = %q, want %q", got, want)
			}
		})
	}
}
