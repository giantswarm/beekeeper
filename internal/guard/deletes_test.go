package guard

import (
	"strings"
	"testing"
)

// deleteCorpus: every command and the target its refusal names, "" for a
// command that passes.
var deleteCorpus = []struct{ cmd, target string }{
	// a target under a variable that may be empty
	{`rm -rf "$D"/*`, `"$D"/*`},
	{`shred -u "$D"/*`, `"$D"/*`},
	{`cd /nowhere && F=x && D=$(mktemp -d -p ~/.local/state/x) && cp a "$D"/plain; shred -u "$D"/*; rmdir "$D"`, `"$D"/*`},
	{`rm -rf $D/*`, `$D/*`},
	{`rm -rf $DIR/`, `$DIR/`},
	{`rm -rf "${X}/cache"`, `"${X}/cache"`},
	{`rm -rf "$D/"`, `"$D/"`},
	{`rm -f -- "$1"/x`, `"$1"/x`},
	{`rm -rf "${D-x}"/y`, `"${D-x}"/y`},
	{`rm -rf "$(git rev-parse --show-toplevel)"/build`, `"$(git rev-parse --show-toplevel)"/build`},
	{"rm -rf `pwd -P`/x", "`pwd -P`/x"},
	{`unlink "$D"/f`, `"$D"/f`},
	{`truncate -s 0 "$D"/log`, `"$D"/log`},
	{`find "$D"/ -name '*.tmp' -delete`, `"$D"/`},
	{`find -L "$D"/x -type f -exec rm -f {} +`, `"$D"/x`},
	{`sudo rm -rf "$D"/*`, `"$D"/*`},
	{`/usr/bin/rm -rf "$D"/*`, `"$D"/*`},
	{`\rm -rf "$D"/*`, `"$D"/*`},
	{`timeout 60 xargs rm -rf "$D"/x`, `"$D"/x`},
	{`bash -c 'rm -rf "$D"/*'`, `"$D"/*`},
	{`zsh -lc "cd x; shred -u $D/*"`, `$D/*`},
	{"bash <<'EOF'\nrm -rf \"$D\"/*\nEOF", `"$D"/*`},
	{`x=$(rm -rf "$D"/* && echo ok)`, `"$D"/*`},
	{`for f in a b; do rm -f "$T"/"$f"; done`, `"$T"/"$f"`},
	{`set -e; rm -rf "$D"/*`, `"$D"/*`},
	// the root, a top-level directory, the home directory, every entry of one
	{`rm -rf /`, `/`},
	{`rm -rf /*`, `/*`},
	{`rm -rf --no-preserve-root /`, `/`},
	{`rm -rf ~`, `~`},
	{`rm -rf ~/`, `~/`},
	{`rm -rf ~/*`, `~/*`},
	{`rm -rf "$HOME"`, `"$HOME"`},
	{`rm -rf "${HOME}/"`, `"${HOME}/"`},
	{`rm -rf $HOME/*`, `$HOME/*`},
	{`rm -rf /home`, `/home`},
	{`rm -rf /usr/`, `/usr/`},
	{`rm -rf /tmp/*`, `/tmp/*`},
	{`rm -rf /var/lib/../..`, `/var/lib/../..`},
	{`rm -rf /tmp/"$D"`, `/tmp/"$D"`},
	{`D=/; rm -rf "$D"`, `"$D"`},
	{`set -u; D=~; rm -rf "$D"`, `"$D"`},
	{`find / -name core -delete`, `/`},
	{`find ~ -exec shred -u {} \;`, `~`},
	// passes
	{`rm -rf "$D"`, ""},
	{`D=$(mktemp -d) && rm -rf "$D"`, ""},
	{`rm -f "${D:?}"/file`, ""},
	{`rm -rf "${D:?missing}"/*`, ""},
	{`set -euo pipefail; rm -f "$D"/file`, ""},
	{`set -u; shred -u "$D"/*`, ""},
	{`set -o nounset; rm -rf $D/x`, ""},
	{`rm -rf "${D:-/tmp/x}"/y`, ""},
	{`rm -rf "$HOME"/.cache/go-build`, ""},
	{`rm -f ~/.local/state/x/app.yaml`, ""},
	{`rm -rf "$PWD"/build`, ""},
	{`rm -rf ./build dist`, ""},
	{`rm -rf /tmp/beekeeper-test`, ""},
	{`rm -f '$D'/x`, ""},
	{`echo "rm -rf $D/*"`, ""},
	{`git rm -r --cached "$D"/x`, ""},
	{`git clean -fdx`, ""},
	{`git -C "$WT" clean -ffdx`, ""},
	{`git worktree remove --force "$WT"`, ""},
	{`klausctl stop agent-1`, ""},
	{`klausctl delete agent-1`, ""},
	{`beekeeper free --apply`, ""},
	{`beekeeper lease release agentlab-1`, ""},
	{`find "$D" -name '*.go'`, ""},
	{`find ~/.cache -name x -print`, ""},
	{`find "${D:?}" -type f -delete`, ""},
	{`cat <<'EOF' > clean.sh` + "\nrm -rf \"$D\"/*\nEOF", ""},
	{`truncate -s 0 ~/.local/state/x/log`, ""},
	{`shred -n 3 -u ./secret.tmp`, ""},
}

func TestDeleteGuardCorpus(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	for _, c := range deleteCorpus {
		d := decide(t, hook(), "/", c.cmd, nil)
		refused := d != nil && d.PermissionDecision == decisionDeny
		switch {
		case c.target == "" && refused:
			t.Errorf("%q is refused:\n%s", c.cmd, d.Reason)
		case c.target == "":
		case !refused:
			t.Errorf("%q: want a refusal, got %+v", c.cmd, d)
		case !strings.Contains(d.Reason, "deletes `"+c.target+"`"):
			t.Errorf("%q: the refusal does not name %q:\n%s", c.cmd, c.target, d.Reason)
		case !strings.Contains(d.Reason, "${D:?}") || !strings.Contains(d.Reason, "set -euo pipefail"):
			t.Errorf("%q: the refusal does not name the safe forms:\n%s", c.cmd, d.Reason)
		}
	}
}

func TestDeleteGuardNamesTheExpansion(t *testing.T) {
	r := deleteRefusal(`cd /nowhere && D=$(mktemp -d) && x; shred -u "$D"/*; rmdir "$D"`)
	for _, want := range []string{"`shred -u \"$D\"/*`", "when `$D` is empty or unset", "that is `/*`"} {
		if !strings.Contains(r, want) {
			t.Errorf("the refusal lacks %q:\n%s", want, r)
		}
	}
}

func TestDeleteGuardHomeByPath(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	for cmd, refused := range map[string]bool{
		"rm -rf /home/u":          true,
		"rm -rf /home/u/*":        true,
		"rm -rf /home/u/.cache/x": false,
		"rm -rf /home/other/x":    false,
	} {
		if got := deleteRefusal(cmd) != ""; got != refused {
			t.Errorf("%q: refused %v, want %v", cmd, got, refused)
		}
	}
}
