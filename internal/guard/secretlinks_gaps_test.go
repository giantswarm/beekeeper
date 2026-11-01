package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secretGapFixture builds the synthetic home of the secret-file gap probes:
// ~/.tool/auth.json and ~/.tool/other.json are configured secret files,
// $HOME/existing-link.json and $WORK/existing-link.json link to the first,
// $WORK/sub is an empty directory and $HOME/profile links to ~/.codex. No
// real credential is involved; commands are inspected, never executed.
func secretGapFixture(t *testing.T) (home, work string, h Hook) {
	t.Helper()
	home, work = t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	for _, p := range []string{filepath.Join(home, ".tool"), filepath.Join(home, ".codex"), filepath.Join(work, "sub")} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{authFixtureName, "other.json"} {
		if err := os.WriteFile(filepath.Join(home, ".tool", name), []byte(`{"token":"synthetic"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{filepath.Join(home, "existing-link.json"), filepath.Join(work, "existing-link.json")} {
		if err := os.Symlink(filepath.Join(home, ".tool", authFixtureName), p); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(home, ".codex"), filepath.Join(home, "profile")); err != nil {
		t.Fatal(err)
	}
	return home, work, Hook{SecretFiles: []string{authFixturePath, otherFixturePath}}
}

// secretGapDecisions runs cmd directly, from a script and from a script with
// --check, through the adapters named, and reports each refusal.
func secretGapDecisions(t *testing.T, h Hook, work, cmd string, adapters []string, want bool) {
	t.Helper()
	cmd = strings.ReplaceAll(cmd, "$WORK", work)
	for _, mode := range []string{secretLinkDirect, secretLinkScript, secretLinkCheck} {
		command := cmd
		if mode != secretLinkDirect {
			p := filepath.Join(work, "probe.sh")
			if err := os.WriteFile(p, []byte(cmd+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			command = bashCmd + " " + ShellQuote(p)
			if mode == secretLinkCheck {
				command += installerCheckArg
			}
		}
		t.Run(mode, func(t *testing.T) {
			for _, adapter := range adapters {
				t.Run(adapter, func(t *testing.T) {
					denied, reason := false, ""
					if adapter == secretLinkCodex {
						d := decideCodexCmd(t, h, work, command)
						denied, reason = d.PermissionDecision == decisionDeny, d.Reason
					} else if d := decide(t, h, work, command, nil); d != nil {
						denied, reason = d.PermissionDecision == decisionDeny, d.Reason
					}
					if denied != want {
						t.Errorf("refused = %v, want %v (%s)", denied, want, reason)
					}
					if denied && reason == "" {
						t.Error("refusal must give a reason")
					}
				})
			}
		})
	}
}

func TestSecretFileGapReads(t *testing.T) {
	_, work, h := secretGapFixture(t)
	both := []string{bashCmd, secretLinkCodex}
	for name, cmd := range map[string]string{
		"dir_link_read":          `ln -s "$HOME/.tool" agentdir; cat agentdir/auth.json`,
		"dir_link_and":           `ln -s "$HOME/.tool" newdir && cat newdir/auth.json`,
		"dir_link_replace":       `ln -sfn "$HOME/.tool" newdir; cat newdir/auth.json`,
		"dir_link_slash":         `ln -s "$HOME/.tool/" newdir; cat newdir/auth.json`,
		"home_link":              `ln -s "$HOME" h; cat h/.tool/auth.json`,
		"cd_relative":            `cd "$HOME/.tool" && ln -s auth.json "$WORK/new.json" && cat "$WORK/new.json"`,
		"relative_existing_link": `ln -s ../existing-link.json sub/link.json; cat sub/link.json`,
		"input_substitution":     `echo "$(<$HOME/.tool/auth.json)"`,
		"input_backtick":         "echo \"` <$HOME/.tool/auth.json `\"",
		"find_follow_exec":       `find -L "$HOME" -name existing-link.json -exec cat {} +`,
		"find_exec":              `find "$HOME/.tool/auth.json" -exec cat {} +`,
		"find_execdir":           `find "$HOME/.tool/auth.json" -execdir cat {} +`,
		"find_ok":                `find "$HOME/.tool/auth.json" -ok cat {} \;`,
		"find_okdir":             `find "$HOME/.tool/auth.json" -okdir cat {} \;`,
		"readlink_substitution":  `cat "$(readlink -f "$HOME/existing-link.json")"`,
		"readlink_xargs":         `readlink -f "$HOME/existing-link.json" | xargs cat`,
		"param_default":          `src="$HOME/.tool/auth.json"; cat "${src:-}"`,
		"param_replace":          `src="$HOME/.tool/auth.json"; cat "${src/a/a}"`,
		"param_concat":           `src="$HOME/.tool/auth.json"; pfx=""; cat "$pfx$src"`,
		"param_indirect":         `src="$HOME/.tool/auth.json"; ref=src; cat "${!ref}"`,
		// Variants beyond the issue's list.
		"dir_link_chain":         `ln -s "$HOME/profile" p2; cat p2/auth.json`,
		"dir_link_target_option": `ln -st newdir "$HOME/.tool"; cat newdir/.tool/auth.json`,
		"dir_link_dot":           `cd "$HOME" && ln -s . h; cat h/.tool/auth.json`,
		"dir_link_helper":        `link() { ln -sfn "${1:?}" "${2:?}"; }; link "$HOME/.tool" d; cat d/auth.json`,
		"dir_link_nested":        `bash -c 'ln -s "$HOME/.tool" d'; cat d/auth.json`,
		"dir_link_jq":            `ln -s "$HOME/.tool" d; jq . d/auth.json`,
		"find_exec_shell":        `find "$HOME/.tool" -name auth.json -exec sh -c 'cat "$1"' _ {} \;`,
		"find_exec_named":        `find . -maxdepth 0 -exec cat "$HOME/.tool/auth.json" \;`,
		"readlink_assigned":      `x=$(readlink -f "$HOME/existing-link.json"); cat "$x"`,
		"realpath_xargs":         `realpath "$HOME/existing-link.json" | xargs -I{} cat {}`,
		"find_print_xargs":       `find "$HOME/.tool" -name auth.json -print0 | xargs -0 cat`,
	} {
		t.Run(name, func(t *testing.T) {
			secretGapDecisions(t, h, work, cmd, both, true)
		})
	}
}

func TestSecretFileGapCodexHomeWrites(t *testing.T) {
	_, work, _ := secretGapFixture(t)
	for name, cmd := range map[string]string{
		"param_operator": `link() { ln -sfn "${1:?}" "${2:?}"; }; link /tmp/input "$HOME/.codex/hooks.json"`,
		"star":           `link() { ln -sfn "$*"; }; link /tmp/input "$HOME/.codex/hooks.json"`,
		"printf_v":       `link() { printf -v d "%s" "$2"; ln -sfn "$1" "$d"; }; link /tmp/input "$HOME/.codex/hooks.json"`,
		"mkdir":          `f() { mkdir -p "$1"; }; f "$HOME/.codex/sub"`,
		"mkdir_linked":   `f() { mkdir -p "$1"; }; f "$HOME/profile/sub"`,
	} {
		t.Run(name, func(t *testing.T) {
			secretGapDecisions(t, Hook{}, work, cmd, []string{secretLinkCodex}, true)
		})
	}
}

// Path and link metadata stays allowed beside the closed gaps.
func TestSecretFileGapMetadata(t *testing.T) {
	_, work, h := secretGapFixture(t)
	both := []string{bashCmd, secretLinkCodex}
	for name, cmd := range map[string]string{
		"dir_link":          `ln -s "$HOME/.tool" agentdir`,
		"dir_link_meta":     `ln -s "$HOME/.tool" agentdir; readlink agentdir; test -L agentdir`,
		"home_link":         `ln -s "$HOME" h`,
		"readlink_f":        `readlink -f "$HOME/existing-link.json"`,
		"readlink_and":      `test -L "$HOME/existing-link.json" && readlink "$HOME/existing-link.json"`,
		"stat":              `stat "$HOME/existing-link.json"`,
		"find_name":         `find -L "$HOME" -name existing-link.json`,
		"find_print":        `find "$HOME/.tool" -name auth.json -print`,
		"find_exec_meta":    `find "$HOME/.tool" -name auth.json -exec ls -l {} +`,
		"find_exec_work":    `find . -name '*.json' -exec wc -c {} +`,
		"helper_mkdir":      `f() { mkdir -p "$1"; }; f "$WORK/sub/deeper"`,
		"echo_substitution": `echo "$(date)"`,
		"pipe_unguarded":    `readlink -f "$WORK/sub" | xargs ls`,
	} {
		t.Run(name, func(t *testing.T) {
			secretGapDecisions(t, h, work, cmd, both, false)
		})
	}
}
