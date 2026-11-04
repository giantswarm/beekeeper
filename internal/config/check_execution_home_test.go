package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Execution workers use a managed HOME, while configuration paths refer to
// BEEKEEPER_EXECUTION_HOME. Check must inspect the same file Load selects.
func TestCheckExecutionHomeMatchesLoad(t *testing.T) {
	for _, exists := range []bool{true, false} {
		name := "only managed home has file"
		if exists {
			name = "only execution home has file"
		}
		t.Run(name, func(t *testing.T) {
			managed, actual := t.TempDir(), t.TempDir()
			t.Setenv("HOME", managed)
			t.Setenv("USERPROFILE", managed)
			t.Setenv("BEEKEEPER_EXECUTION_HOME", actual)
			fileHome := managed
			if exists {
				fileHome = actual
			}
			const input = "fixture.toml"
			if err := os.WriteFile(filepath.Join(fileHome, input), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			p := writeConfig(t, "codex: {configFile: ~/"+input+"}\n")
			c, err := Load(p)
			if err != nil || c.Codex.ConfigFile != filepath.Join(actual, input) {
				t.Fatalf("Load selected wrong input: %v, %v", c, err)
			}
			findings, err := Check(p)
			if err != nil {
				t.Fatal(err)
			}
			if exists && len(findings) != 0 || !exists && (len(findings) != 1 || !strings.HasPrefix(findings[0], "codex.configFile: referenced file does not exist")) {
				t.Fatalf("Check = %v, input exists at loaded path: %v", findings, exists)
			}
		})
	}
}
