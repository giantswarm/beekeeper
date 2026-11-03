package guard

import (
	"os"
	"path/filepath"
	"testing"
)

func secretShapeFixture(t *testing.T) string {
	t.Helper()
	home := madeLinkFixture(t)
	if err := os.MkdirAll(filepath.Join(home, ".tool"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".tool", "auth.json"), []byte(`{"token":"synthetic"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func secretShapeDecisions(t *testing.T, home, cmd string, adapters []string, want bool) {
	t.Helper()
	h := Hook{SecretFiles: []string{filepath.Join(home, ".tool", "auth.json")}}
	for _, mode := range []string{secretLinkDirect, secretLinkScript, secretLinkCheck} {
		command := cmd
		if mode != secretLinkDirect {
			p := filepath.Join(home, "probe.sh")
			if err := os.WriteFile(p, []byte("#!/bin/bash\n"+cmd+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			command = "bash " + ShellQuote(p) + " --pull"
			if mode == secretLinkCheck {
				command = "bash " + ShellQuote(p) + installerCheckArg
			}
		}
		t.Run(mode, func(t *testing.T) {
			for _, adapter := range adapters {
				t.Run(adapter, func(t *testing.T) {
					denied := false
					if adapter == secretLinkCodex {
						denied = decideCodexCmd(t, h, home, command).PermissionDecision == decisionDeny
					} else if d := decide(t, h, home, command, nil); d != nil {
						denied = d.PermissionDecision == decisionDeny
					}
					if denied != want {
						t.Errorf("refused = %v, want %v", denied, want)
					}
				})
			}
		})
	}
}

func TestCodexSplitHomeHelper(t *testing.T) {
	home := secretShapeFixture(t)
	for _, cmd := range []string{
		`helper "$HOME/." co dex/hooks.json`,
		`helper "$HOME" .codex hooks.json`,
		`helper --write "$HOME/." co dex/hooks.json`,
	} {
		t.Run(cmd, func(t *testing.T) {
			if d := decideCodexCmd(t, Hook{}, home, cmd); d.PermissionDecision != decisionDeny {
				t.Errorf("split protected-home helper allowed: %+v", d)
			}
		})
	}
}

// Inspect synthetic commands only; none of the copies, moves or helpers runs.
func TestSecretCommandShapes(t *testing.T) {
	home := secretShapeFixture(t)
	both := []string{bashCmd, secretLinkCodex}
	for _, tc := range []struct{ name, command string }{
		{"pushd", `pushd "$HOME/.tool"; ln -s auth.json "$HOME/new.json"; cat "$HOME/new.json"`},
		{"popd", `pushd "$HOME/.tool"; pushd "$HOME"; popd; ln -s auth.json "$HOME/new.json"; cat "$HOME/new.json"`},
		{"copy_directory_links", `cp -rs "$HOME/.tool" "$HOME/copy"; cat "$HOME/copy/auth.json"`},
		{"copy_directory_long", `cp --recursive --symbolic-link "$HOME/.tool" "$HOME/copy"; cat "$HOME/copy/auth.json"`},
		{"find_move", `find "$HOME/.tool/auth.json" -exec mv {} "$HOME/moved.json" \; ; cat "$HOME/moved.json"`},
		{"find_copy", `find "$HOME/.tool/auth.json" -exec cp {} "$HOME/moved.json" \; ; cat "$HOME/moved.json"`},
		{"split_helper", `helper "$HOME/." tool/ auth.json`},
	} {
		t.Run(tc.name, func(t *testing.T) { secretShapeDecisions(t, home, tc.command, both, true) })
	}
}

func TestSecretCommandShapeMetadata(t *testing.T) {
	home := secretShapeFixture(t)
	for _, command := range []string{
		`pushd "$HOME/.tool"; stat auth.json; popd`,
		`find "$HOME/.tool/auth.json" -exec stat {} \;`,
		`find "$HOME/.tool/auth.json" -exec echo {} \;`,
		`cp -rs "$HOME/public" "$HOME/copy"; cat "$HOME/copy/readme"`,
	} {
		t.Run(command, func(t *testing.T) { secretShapeDecisions(t, home, command, []string{bashCmd, secretLinkCodex}, false) })
	}
}
