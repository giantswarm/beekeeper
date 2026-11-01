package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installerHelper is an install script's link helper: more than one ln, so
// the simple link allowance does not cover it.
const installerHelper = `link() {
    local src="$1" dst="$2"
    if [[ -L "$dst" && "$(readlink -f "$dst")" == "$(readlink -f "$src")" ]]; then
        echo "linked $dst"
        return 0
    fi
    mkdir -p "$(dirname "$dst")"
    if [[ -e "$dst" || -L "$dst" ]]; then
        mv -n "$dst" "$dst.before"
        echo "moved the previous $dst aside"
    fi
    ln -sn "$src" "$dst"
    echo "linked $dst"
}
`

// madeLinkFixture is a synthetic home with ~/.codex/auth.json, a built-in
// secret file, a hard link of it at ~/hard.json, and a separate CODEX_HOME.
// Commands are inspected, never executed.
func madeLinkFixture(t *testing.T) (home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".local", "state", "tool", "codex"))
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(home, ".codex", "auth.json")
	if err := os.WriteFile(auth, []byte(`{"token":"synthetic"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(auth, filepath.Join(home, "hard.json")); err != nil {
		t.Fatal(err)
	}
	return home
}

// madeLinkDecisions runs cmd directly, from a script run with an argument
// and from a script run with --check, through the adapters named.
func madeLinkDecisions(t *testing.T, home, cmd string, adapters []string, want bool) {
	t.Helper()
	h := Hook{}
	for _, mode := range []string{secretLinkDirect, secretLinkScript, secretLinkCheck} {
		command := cmd
		if mode != secretLinkDirect {
			p := filepath.Join(home, "install.sh")
			if err := os.WriteFile(p, []byte("#!/bin/bash\n"+cmd+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			command = bashCmd + " " + ShellQuote(p) + " --pull"
			if mode == secretLinkCheck {
				command = bashCmd + " " + ShellQuote(p) + installerCheckArg
			}
		}
		t.Run(mode, func(t *testing.T) {
			for _, adapter := range adapters {
				t.Run(adapter, func(t *testing.T) {
					denied, reason := false, ""
					if adapter == secretLinkCodex {
						d := decideCodexCmd(t, h, home, command)
						denied, reason = d.PermissionDecision == decisionDeny, d.Reason
					} else if d := decide(t, h, home, command, nil); d != nil {
						denied, reason = d.PermissionDecision == decisionDeny, d.Reason
					}
					if denied != want {
						t.Errorf("refused = %v, want %v: %s", denied, want, reason)
					}
				})
			}
		})
	}
}

// https://github.com/giantswarm/beekeeper/issues/1898: a link to a credential
// file reads none of it and passes wherever it stands; a read through the link
// stays refused.
func TestSecretLinkMadeAnywhere(t *testing.T) {
	home := madeLinkFixture(t)
	both := []string{bashCmd, secretLinkCodex}
	const dest = `D="$HOME/.local/state/desk/codex"
mkdir -p "$D"
`
	for _, tc := range []struct {
		name, command string
		adapters      []string
		deny          bool
	}{
		// the issue: an install script that links the credential file
		{"script_symlink", dest + `echo "linking"
ln -s "$HOME/.codex/auth.json" "$D/auth.json"
git status --short`, both, false},
		{"defaulted_destination", `D="${DESK_HOME:-$HOME/.local/state}/desk/codex"
mkdir -p "$D"
ln -sfn "$HOME/.codex/auth.json" "$D/auth.json"`, both, false},
		{"hard_link", dest + `ln "$HOME/.codex/auth.json" "$D/auth.json"`, both, false},
		{"link_command", dest + `link "$HOME/.codex/auth.json" "$D/auth.json"`, both, false},
		{"into_directory", dest + `ln -s -t "$D" "$HOME/.codex/auth.json"`, both, false},
		{"helper_call", installerHelper + dest + `say() { printf '\n==> %s\n' "$*"; }
say linking
case "${1:-}" in
    --pull) git pull --ff-only ;;
    *) ;;
esac
for f in *.md; do echo "$f"; done
if [[ -f "$HOME/.codex/auth.json" ]]; then
    link "$HOME/.codex/auth.json" "$D/auth.json"
fi`, []string{bashCmd}, false},
		{"nested_helper", `inner() { ln -sfn "$1" "$2"; }; link() { inner "$1" "$2"; }; link "$HOME/.codex/auth.json" new.json`, []string{bashCmd}, false},

		// reading through the new link stays refused
		{"read_link", dest + `ln -s "$HOME/.codex/auth.json" "$D/auth.json"
cat "$D/auth.json"`, both, true},
		{"read_literal_path", dest + `ln -s "$HOME/.codex/auth.json" "$D/auth.json"
cat ~/.local/state/desk/codex/auth.json`, both, true},
		{"read_before_link", dest + `cat "$D/auth.json"
ln -s "$HOME/.codex/auth.json" "$D/auth.json"`, both, true},
		{"read_alias", dest + `ln -s "$HOME/.codex/auth.json" "$D/auth.json"
f="$D/auth.json"
cat "$f"`, both, true},
		{"read_glob", dest + `ln -s "$HOME/.codex/auth.json" "$D/auth.json"
cat "$D"/*`, both, true},
		{"read_recursive", dest + `ln -s "$HOME/.codex/auth.json" "$D/auth.json"
grep -r token "$D"`, both, true},
		{"read_into_directory", dest + `ln -s -t "$D" "$HOME/.codex/auth.json"
cat "$D/auth.json"`, both, true},
		{"read_hard_link", dest + `ln "$HOME/.codex/auth.json" "$D/auth.json"
base64 "$D/auth.json"`, both, true},
		{"read_strip", `ln -s "$HOME/.codex/auth.json" new.json; jq 'del(.. | .token?)' new.json`, both, true},
		{"read_python", `ln -s "$HOME/.codex/auth.json" new.json; python3 -c 'print(open("new.json").read())'`, both, true},
		{"read_nested", `bash -c 'ln -s "$HOME/.codex/auth.json" new.json'; cat new.json`, both, true},
		{"read_chain", `ln -s "$HOME/.codex/auth.json" one.json; ln -s one.json two.json; cat two.json`, both, true},
		{"read_linked_directory", `mkdir -p sub; ln -s "$HOME/.codex/auth.json" sub/auth.json; ln -s sub dir; cat dir/auth.json`, both, true},
		{"read_helper_link", installerHelper + dest + `link "$HOME/.codex/auth.json" "$D/auth.json"
cat "$D/auth.json"`, both, true},
		{"helper_reads", `link() { ln -s "$1" "$2"; cat "$2"; }; link "$HOME/.codex/auth.json" new.json`, both, true},
		{"helper_reads_source", `link() { local src="$1"; ln -s "$src" "$2"; head "$src"; }; link "$HOME/.codex/auth.json" new.json`, both, true},
		{"helper_prints_args_to_reader", `link() { ln -s "$1" "$2"; printf '%s\n' "$@" | xargs cat; }; link "$HOME/.codex/auth.json" new.json`, both, true},
		{"helper_local_substitution_read", `helper() {
    local value="$(cat "$1")"
    printf '%s' "$value"
    ln -s "$1" "$2"
}
helper "$HOME/.codex/auth.json" new.json`, both, true},
		{"helper_plain_substitution_read", `helper() {
    value="$(cat "$1")"
    printf '%s' "$value"
    ln -s "$1" "$2"
}
helper "$HOME/.codex/auth.json" new.json`, both, true},
		{"helper_backtick_read", "helper() { local value=`head \"$1\"`; ln -s \"$1\" \"$2\"; }; helper \"$HOME/.codex/auth.json\" new.json", both, true},
		{"helper_default_substitution_read", `helper() { local value="${X:-$(base64 "$1")}"; ln -s "$1" "$2"; }; helper "$HOME/.codex/auth.json" new.json`, both, true},
		{"helper_substitution_reads_link", `helper() {
    ln -s "$1" "$2"
    local value="$(cat "$2")"
    printf '%s' "$value"
}
helper "$HOME/.codex/auth.json" new.json`, both, true},
		{"helper_plain_substitution_reads_link", `helper() { ln -s "$1" "$2"; value=$(jq . "$2"); echo "$value"; }; helper "$HOME/.codex/auth.json" new.json`, both, true},
		// a metadata path a substitution assigns may be read later: refused, as
		// x="$(dirname <credential file>)" is
		{"helper_assigned_metadata_substitution", `helper() { local dir="$(dirname "$2")"; mkdir -p "$dir"; ln -s "$1" "$2"; }; helper "$HOME/.codex/auth.json" "$HOME/.local/state/tool/links/auth.json"`, []string{bashCmd}, true},
		{"helper_recursive", `link() { link "$1" "$2"; }; link "$HOME/.codex/auth.json" new.json`, both, true},
		{"helper_shadowed_ln", `ln() { cat "$2"; }; link() { ln -s "$1" "$2"; }; link "$HOME/.codex/auth.json" new.json`, both, true},

		// links that cannot pass
		{"directory_source", `ln -s "$HOME/.codex" codex; echo done`, both, true},
		{"glob_source", `ln -s "$HOME/.codex/"*.json here/; echo done`, both, true},
		{"substituted_destination", `ln -s "$HOME/.codex/auth.json" "$(mktemp)"; echo done`, both, true},
		{"backtick_destination", "ln -s \"$HOME/.codex/auth.json\" `mktemp`; echo done", both, true},
		{"option_destination", `ln -s -t -x "$HOME/.codex/auth.json"; echo done`, both, true},
		{"copy", `cp "$HOME/.codex/auth.json" copy.json; echo done`, both, true},

		// an existing hard link of a guarded file is the guarded file
		{"existing_hard_link", `cat "$HOME/hard.json"`, both, true},
		{"existing_hard_link_metadata", `stat "$HOME/hard.json"`, both, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			madeLinkDecisions(t, home, tc.command, tc.adapters, tc.deny)
		})
	}
}

// The tests an install script runs around a link: a [[ … ]] split at its &&
// or || is still the test, a substitution in it passes its path to the test,
// and mkdir takes a path without opening it; a reader keeps the refusal.
func TestSecretLinkTestExpressions(t *testing.T) {
	home := madeLinkFixture(t)
	for _, tc := range []struct {
		name, command string
		deny          bool
	}{
		{"test_or", `[[ -e "$HOME/.codex/auth.json" || -L "$HOME/.codex/auth.json" ]] && echo yes`, false},
		{"test_substitution", `[[ -L x && "$(readlink -f x)" == "$(readlink -f "$HOME/.codex/auth.json")" ]] && echo same`, false},
		{"mkdir_dirname", `mkdir -p "$(dirname "$HOME/.codex/auth.json")"`, false},
		{"test_nested_reader", `[[ -n "$(cat "$(readlink -f "$HOME/.codex/auth.json")")" ]]`, true},
		{"test_then_reader", `[[ -n x ]] && cat "$(readlink -f "$HOME/.codex/auth.json")"`, true},
		{"test_reader", `[[ -n x || $(cat "$HOME/.codex/auth.json") ]]`, true},
		{"closed_test_reader", `[[ -n x ]] || head "$HOME/.codex/auth.json"`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := decide(t, Hook{}, home, tc.command, nil)
			denied := d != nil && d.PermissionDecision == decisionDeny
			if denied != tc.deny {
				reason := ""
				if d != nil {
					reason = d.Reason
				}
				t.Errorf("refused = %v, want %v: %s", denied, tc.deny, strings.TrimSpace(reason))
			}
		})
	}
}
