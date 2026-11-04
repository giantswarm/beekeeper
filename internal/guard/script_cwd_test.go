package guard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSecretScriptsResolveAfterChdir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	sub := filepath.Join(dir, "scripts")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(dir, "fixture-config.json")
	for path, content := range map[string]string{
		protected:                      `{"password":"synthetic-only"}`,
		filepath.Join(sub, "read.sh"):  "#!/bin/sh\ncat " + ShellQuote(protected) + "\n",
		filepath.Join(sub, "clean.sh"): "#!/bin/sh\nprintf fixture\n",
	} {
		mode := os.FileMode(0o600)
		if path != protected {
			mode = 0o700 // Executable fixtures for the direct ./read.sh invocation.
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	h := Hook{SecretFiles: []string{protected}}
	for _, tc := range []struct {
		command string
		deny    bool
	}{
		{"bash " + ShellQuote(filepath.Join(sub, "read.sh")), true},
		{"cd scripts && bash read.sh", true},
		{"cd scripts && ./read.sh", true},
		{"cd scripts && . ./read.sh", true},
		{"cd scripts && bash <read.sh", true},
		{"bash -c 'cd scripts && bash read.sh'", true},
		{"cd scripts && bash -n read.sh", false},
		{"cd scripts && cat read.sh", false},
		{"cd scripts && bash clean.sh", false},
	} {
		t.Run(tc.command, func(t *testing.T) {
			// All adapters inspect fixture text only; no script is executed.
			d := decide(t, h, dir, tc.command, nil)
			if got := d != nil && d.PermissionDecision == decisionDeny; got != tc.deny {
				t.Errorf("Claude refused = %v, want %v", got, tc.deny)
			}
			if got := decideAgyCmd(t, h, dir, tc.command).Decision == decisionDeny; got != tc.deny {
				t.Errorf("agy refused = %v, want %v", got, tc.deny)
			}
			if got := decideCodexCmd(t, h, dir, tc.command).PermissionDecision == decisionDeny; got != tc.deny {
				t.Errorf("Codex refused = %v, want %v", got, tc.deny)
			}
		})
	}
}
