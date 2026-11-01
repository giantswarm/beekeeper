// Package testenv isolates test binaries from the desk that starts them.
package testenv

import (
	"os"
	"strings"
)

// Setup runs before m.Run, after any test-binary helper dispatch. Tests set
// their own identity with t.Setenv; child helpers keep that explicit identity.
// BEEKEEPER_TEST_* includes integration inputs and child-helper dispatch flags;
// clearing the latter would turn a helper into another test-suite run.
func Setup() {
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "BEEKEEPER_TEST_") {
			continue
		}
		for _, prefix := range []string{"BEEKEEPER_", "CLAUDE_", "CODEX_", "GIT_"} {
			if strings.HasPrefix(key, prefix) {
				if err := os.Unsetenv(key); err != nil {
					panic(err)
				}
				break
			}
		}
	}
	// Git commands, including those in shell fixtures, use only test settings.
	for key, value := range map[string]string{
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_COUNT":    "3",
		"GIT_CONFIG_KEY_0":    "user.name", "GIT_CONFIG_VALUE_0": "Test",
		"GIT_CONFIG_KEY_1": "user.email", "GIT_CONFIG_VALUE_1": "test@example.com",
		"GIT_CONFIG_KEY_2": "commit.gpgsign", "GIT_CONFIG_VALUE_2": "false",
	} {
		if err := os.Setenv(key, value); err != nil {
			panic(err)
		}
	}
}
