package guard

import (
	"os"
	"path/filepath"
	"testing"
)

// Only synthetic files are used; commands are inspected, never executed.
func TestSecretLinkOperations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".tool"))
	if err := os.MkdirAll(filepath.Join(home, ".tool"), 0o700); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(home, ".tool", "auth.json")
	if err := os.WriteFile(auth, []byte(`{"token":"synthetic"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(auth, filepath.Join(home, "profile.json")); err != nil {
		t.Fatal(err)
	}
	h := Hook{SecretFiles: []string{"~/.tool/auth.json"}}
	const secret = `"$HOME/.tool/auth.json"`
	const profile = `"$HOME/profile.json"`
	for _, tc := range []struct {
		name, command string
		deny          bool
	}{
		{"create", "ln -s " + secret + " new.json", false},
		{"replace", "ln -sfn " + secret + " " + profile, false},
		{"long_option", "ln --symbolic --force --no-dereference " + secret + " " + profile, false},
		{"readlink", "readlink " + profile, false},
		{"test_L", "test -L " + profile, false},
		{"test_e", "test -e " + secret, false},
		{"test_h", "test -h " + profile, false},
		{"bracket_e", "[ -e " + secret + " ]", false},
		{"stat", "stat " + profile, false},
		{"grep_count", "grep -c token " + secret, false},
		{"grep_files", "grep -l token " + secret, false},
		{"keys", "jq keys " + secret, false},
		{"cat", "cat " + secret, true},
		{"copy", "cp " + secret + " copy.json", true},
		{"source", "source " + secret, true},
		{"dot", ". " + secret, true},
		{"redirect", "cat < " + secret, true},
		{"metadata_redirect", "stat " + profile + " < " + secret, true},
		{"grep_lines", "grep token " + secret, true},
		{"base64", "base64 " + secret, true},
		{"head", "head " + secret, true},
		{"tail", "tail " + secret, true},
		{"copy_dereference", "cp -L " + profile + " copy.json", true},
		{"rsync_dereference", "rsync -L " + profile + " copy.json", true},
		{"tar", "tar -cf archive.tar " + secret, true},
		{"zip", "zip archive.zip " + secret, true},
		{"read_link", "cat " + profile, true},
		{"hard_link", "ln " + secret + " new.json", true},
		{"option_operand", "ln -t -s " + secret, true},
		{"link_and_read", "ln -s " + secret + "; cat " + secret, true},
		{"new_link_and_read", "ln -s " + secret + " new.json; cat new.json", true},
		{"helper", `link() { ln -sfn "$1" "$2"; }; link ` + secret + " " + profile, false},
		{"helper_check", `link() {
	local src="$1" dst="$2"
	mkdir -p "$(dirname "$dst")"
	if [ -e "$src" ]; then
		test -L "$dst" && [ "$(readlink "$dst")" = "$src" ]
	else
		ln -sfn "$src" "$dst"
	fi
	printf '%s\n' "$dst"
}; link ` + secret + " " + profile, false},
		{"helper_read", `link() { ln -sfn "$1" "$2"; cat "$1"; }; link ` + secret + " " + profile, true},
		{"helper_redirect", `link() { ln -sfn "$1" "$2" < "$1"; }; link ` + secret + " " + profile, true},
		{"helper_unknown", `link() { unknown "$1"; }; link ` + secret, true},
		{"helper_nested", `inner() { ln -sfn "$1" "$2"; }; link() { inner "$1" "$2"; }; link ` + secret + " " + profile, false},
		{"helper_new_link_read", `link() { local src="$1" dst="$2"; ln -sfn "$src" "$dst"; }; link ` + secret + ` new.json; cat new.json`, true},
		{"helper_redefined", `link() { ln -sfn "$1" "$2"; }; link() { cat "$1"; }; link ` + secret, true},
		{"helper_shadow", `ln() { cat "$1"; }; link() { ln -sfn "$1" "$2"; }; link ` + secret + " " + profile, true},
		{"helper_substitution", `link() { ln -sfn "$(cat "$1")" "$2"; }; link ` + secret + " " + profile, true},
		{"helper_recursive", `link() { link "$1" "$2"; }; link ` + secret + " " + profile, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"direct", "script", "check"} {
				command := tc.command
				if mode != "direct" {
					p := filepath.Join(home, "installer.sh")
					if err := os.WriteFile(p, []byte("#!/bin/sh\n"+command+"\n"), 0o700); err != nil {
						t.Fatal(err)
					}
					command = "bash " + ShellQuote(p)
					if mode == "check" {
						command += " --check"
					}
				}
				t.Run(mode, func(t *testing.T) {
					for _, adapter := range []string{"bash", "codex"} {
						t.Run(adapter, func(t *testing.T) {
							var denied bool
							var reason string
							if adapter == "codex" {
								d := decideCodexCmd(t, h, home, command)
								denied, reason = d.PermissionDecision == decisionDeny, d.Reason
							} else if d := decide(t, h, home, command, nil); d != nil {
								denied, reason = d.PermissionDecision == decisionDeny, d.Reason
							}
							if denied != tc.deny {
								t.Errorf("refused = %v, want %v", denied, tc.deny)
							}
							if denied && reason == "" {
								t.Error("refusal must give a reason")
							}
						})
					}
				})
			}
		})
	}
}

func TestSecretLinkCodexHomeWrites(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".codex"), filepath.Join(home, "profile")); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		`ln -s config.json "$HOME/.codex/hooks.json"`,
		`ln -s config.json "$HOME/profile/hooks.json"`,
		`ln -s config.json "$HOME/profile"`,
		`ln -st "$HOME/profile" config.json`,
	} {
		if d := decideCodexCmd(t, Hook{}, home, cmd); d.PermissionDecision != decisionDeny {
			t.Errorf("Codex home link write passed: %s", cmd)
		}
	}
}
