package guard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSecretStaticScript(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CODEX_HOME", filepath.Join(dir, ".codex"))
	script := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat \"$HOME/.codex/auth.json\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		command string
		deny    bool
	}{
		{"bash -n install.sh", false},
		{"sh -n install.sh", false},
		{"bash -nv install.sh", false},
		{"bash -o noexec install.sh", false},
		{"shellcheck install.sh", false},
		{"bash install.sh --pull", true},
		{"bash -n +n install.sh", true},
		{"bash -ni install.sh", true},
		{"bash install.sh -n", true},
		{"bash -n install.sh +n", false},
		{"bash -n -- install.sh", false},
		{"bash -n install.sh; cat ~/.codex/auth.json", true},
		{"bash -n $(cat ~/.codex/auth.json)", true},
		{"bash -n < ~/.codex/auth.json", true},
		{"shellcheck ~/.codex/auth.json", true},
	} {
		t.Run(tc.command, func(t *testing.T) {
			d := decide(t, Hook{}, dir, tc.command, nil)
			denied := d != nil && d.PermissionDecision == decisionDeny
			if denied != tc.deny {
				t.Errorf("denied = %v, want %v", denied, tc.deny)
			}
		})
	}
}

func TestSecretScriptAfterEndOfOptions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CODEX_HOME", filepath.Join(dir, ".codex"))
	for _, name := range []string{"+payload.sh", "-payload.sh", "-c"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\ncat \"$HOME/.codex/auth.json\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, spelling := range []string{name, "./" + name} {
			command := "bash -- " + spelling
			t.Run(command, func(t *testing.T) {
				if d := decide(t, Hook{}, dir, command, nil); d == nil || d.PermissionDecision != decisionDeny {
					t.Error("executed script containing a credential read was allowed")
				}
			})
		}
	}
}
