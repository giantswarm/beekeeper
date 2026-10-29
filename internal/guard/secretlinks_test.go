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
	h := Hook{SecretFiles: []string{authFixturePath}}
	const sourcePath = `"$HOME/.tool/auth.json"`
	const profile = `"$HOME/profile.json"`
	for _, tc := range []struct {
		name, command string
		deny          bool
	}{
		{"create", "ln -s " + sourcePath + " new.json", false},
		{"force", "ln -sf " + sourcePath + " " + profile, false},
		{"replace", "ln -sfn " + sourcePath + " " + profile, false},
		{"long_option", "ln --symbolic --force --no-dereference " + sourcePath + " " + profile, true},
		{"readlink", "readlink " + profile, false},
		{"test_L", "test -L " + profile, false},
		{"test_e", "test -e " + sourcePath, false},
		{"test_h", "test -h " + profile, false},
		{"bracket_e", "[ -e " + sourcePath + " ]", false},
		{"stat", "stat " + profile, false},
		{"grep_count", "grep -c token " + sourcePath, false},
		{"grep_files", "grep -l token " + sourcePath, false},
		{"keys", "jq keys " + sourcePath, false},
		{"cat", "cat " + sourcePath, true},
		{"copy", "cp " + sourcePath + " copy.json", true},
		{"source", "source " + sourcePath, true},
		{"dot", ". " + sourcePath, true},
		{"redirect", "cat < " + sourcePath, true},
		{"metadata_redirect", "stat " + profile + " < " + sourcePath, true},
		{"grep_lines", "grep token " + sourcePath, true},
		{"base64", "base64 " + sourcePath, true},
		{"head", "head " + sourcePath, true},
		{"tail", "tail " + sourcePath, true},
		{"copy_dereference", "cp -L " + profile + " copy.json", true},
		{"rsync_dereference", "rsync -L " + profile + " copy.json", true},
		{"tar", "tar -cf archive.tar " + sourcePath, true},
		{"zip", "zip archive.zip " + sourcePath, true},
		{"read_link", "cat " + profile, true},
		{"hard_link", "ln " + sourcePath + " new.json", true},
		{"option_operand", "ln -t -s " + sourcePath, true},
		{"link_and_read", "ln -s " + sourcePath + "; cat " + sourcePath, true},
		{"new_link_and_read", "ln -s " + sourcePath + " new.json; cat new.json", true},
		{"new_link_wrong_strip", "ln -s " + sourcePath + ` new.json; cat new.json | jq 'del(.. | .apiKey?)'`, true},
		{"new_link_strip", "ln -s " + sourcePath + ` new.json; cat new.json | jq 'del(.. | .token?)'`, true},
		{"variable_link_read", `src=` + sourcePath + `; dst=new.json; ln -s "$src" "$dst"; cat new.json`, true},
		{"helper", `link() { ln -sfn "$1" "$2"; }; link ` + sourcePath + " " + profile, false},
		{"helper_create", `link() { ln -s "$1" "$2"; }; link ` + sourcePath + " " + profile, false},
		{"helper_force", `link() { ln -sf "$1" "$2"; }; link ` + sourcePath + " " + profile, false},
		{"assigned_helper", `link() { ln -sfn "$1" "$2"; }; src=` + sourcePath + `; dst=new.json; link "$src" "$dst"`, false},
		{"metadata_checks", "ln -s " + sourcePath + ` new.json; readlink new.json; test -L new.json; [ -e new.json ]`, false},
		{"helper_check", `link() {
	local src="$1" dst="$2"
	mkdir -p "$(dirname "$dst")"
	if [ -e "$src" ]; then
		test -L "$dst" && [ "$(readlink "$dst")" = "$src" ]
	else
		ln -sfn "$src" "$dst"
	fi
	printf '%s\n' "$dst"
}; link ` + sourcePath + " " + profile, true},
		{"helper_read", `link() { ln -sfn "$1" "$2"; cat "$1"; }; link ` + sourcePath + " " + profile, true},
		{"helper_redirect", `link() { ln -sfn "$1" "$2" < "$1"; }; link ` + sourcePath + " " + profile, true},
		{"helper_unknown", `link() { unknown "$1"; }; link ` + sourcePath, true},
		{"helper_nested", `inner() { ln -sfn "$1" "$2"; }; link() { inner "$1" "$2"; }; link ` + sourcePath + " " + profile, true},
		{"helper_spread", `link() { ln -sfn "$@"; }; link ` + sourcePath + " " + profile, false},
		{"helper_spread_read", `link() { ln -sfn "$@"; }; link ` + sourcePath + ` new.json; cat new.json`, true},
		{"helper_global_read", `src=` + sourcePath + `; link() { ln -sfn "$src" "$1"; }; link new.json; cat new.json`, true},
		{"helper_new_link_read", `link() { local src="$1" dst="$2"; ln -sfn "$src" "$dst"; }; link ` + sourcePath + ` new.json; cat new.json`, true},
		{"helper_redefined", `link() { ln -sfn "$1" "$2"; }; link() { cat "$1"; }; link ` + sourcePath, true},
		{"helper_shadow", `ln() { cat "$1"; }; link() { ln -sfn "$1" "$2"; }; link ` + sourcePath + " " + profile, true},
		{"helper_substitution", `link() { ln -sfn "$(cat "$1")" "$2"; }; link ` + sourcePath + " " + profile, true},
		{"helper_recursive", `link() { link "$1" "$2"; }; link ` + sourcePath + " " + profile, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{secretLinkDirect, secretLinkScript, secretLinkCheck} {
				command := tc.command
				if mode != secretLinkDirect {
					p := filepath.Join(home, "installer.sh")
					if err := os.WriteFile(p, []byte("#!/bin/sh\n"+command+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					command = bashCmd + " " + ShellQuote(p)
					if mode == secretLinkCheck {
						command += " --check"
					}
				}
				t.Run(mode, func(t *testing.T) {
					for _, adapter := range []string{bashCmd, secretLinkCodex} {
						t.Run(adapter, func(t *testing.T) {
							var denied bool
							var reason string
							if adapter == secretLinkCodex {
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
		`link() { ln -sfn "$1" "$2"; }; link "$HOME/.codex/auth.json" "$HOME/.codex/hooks.json"`,
		`link() { ln -sfn "$1" "$2"; }; link "$HOME/.codex/auth.json" "$HOME/profile/hooks.json"`,
		`link() { ln -sfn "$@"; }; link "$HOME/.codex/auth.json" "$HOME/.codex/hooks.json"`,
	} {
		if d := decideCodexCmd(t, Hook{}, home, cmd); d.PermissionDecision != decisionDeny {
			t.Errorf("Codex home link write passed: %s", cmd)
		}
	}
}
