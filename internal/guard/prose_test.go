package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/lease"
)

// TestHookLeavesProseUnrefused: the hook keys on the commands a call
// executes, never on prose that names them. A here-document, a quoted string
// or an interpreter program's string literal or comment that only quotes a
// blocking devctl command (a CHANGELOG entry, README text, a PR body draft) is
// not refused; the same command run by the program or the shell still is
// (https://github.com/giantswarm/beekeeper/issues/1033).
func TestHookLeavesProseUnrefused(t *testing.T) {
	h := Hook{Self: self, Shell: testShell, Clusters: func() []string { return nil }, Leases: func() []lease.Holder { return nil }}
	g := self + " gate -- "
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docs.py"), []byte("import pathlib\n"+
		"p = pathlib.Path('README.md')\n"+
		"s = p.read_text()\n"+
		"s = s.replace('old', \"A worker waits with `devctl pr wait o/r 7` and lands it with devctl pr merge o/r 7 --update-branch.\")\n"+
		"# devctl release promote o/r runs after the proof\n"+
		"p.write_text(s)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		// A python3 here-document whose document text quotes the merge flags.
		"python3 - <<'EOF'\nimport pathlib\np = pathlib.Path('docs/merge.md')\np.write_text('''Merge an upstream line with\n\n    devctl pr merge o/r 7 --rebase --update-branch\n\nthrough the gate.\n''')\nEOF",
		// CHANGELOG prose naming promote and release wait.
		"python3 - <<'EOF'\nimport re\nc = open('CHANGELOG.md').read()\nc = c.replace('## [Unreleased]', \"## [Unreleased]\\n\\n- The gate runs devctl release promote o/r and devctl release wait o/r 1.2.3.\")\nopen('CHANGELOG.md', 'w').write(c)\nEOF",
		// A scratch script's string literals and comments.
		"python3 -I docs.py",
		"python3 -c 'print(\"wait with devctl pr wait o/r 7\")'",
		"node -e 'console.log(`see devctl rollout wait inst o/r --pr 1`)'",
		// A PR body draft in a here-document, sent by gh api.
		"gh api -X PATCH repos/o/r/pulls/7 -f body=\"$(cat <<'EOF'\n## Proof\n\nThe merge ran as devctl pr wait o/r 7, then devctl pr merge o/r 7 --rebase.\nEOF\n)\"",
		"cat >| body.md <<'EOF'\nNever run bash -c 'devctl pr merge o/r 7' or eval devctl pr wait o/r 7.\nEOF",
		"gh pr comment 7 --repo o/r --body 'bash -c \"devctl pr merge o/r 7\" is refused'",
	} {
		if d := decide(t, h, dir, cmd, nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("%q is refused: %s", cmd, d.Reason)
		}
	}
	for _, c := range []struct{ cmd, want string }{
		// The same commands as commands are refused as before.
		{"python3 - <<'EOF'\nimport subprocess\nsubprocess.run(['devctl', 'pr', 'merge', 'o/r', '7', '--rebase'], check=True)\nEOF", "a devctl pr merge in the here-document python3 reads"},
		{"python3 - <<'EOF'\nimport subprocess\nsubprocess.run('cd x && devctl pr merge o/r 7 --rebase', shell=True)\nEOF", "line 2: subprocess.run('cd x && devctl pr merge o/r 7 --rebase', shell=True)"},
		{"python3 -c 'import os; os.system(\"devctl release promote o/r\")'", "a devctl release promote in python3's code (-c)"},
		{"perl -e 'print `devctl pr wait o/r 7`'", "a devctl pr wait in perl's code (-e)"},
		{"node -e 'require(\"child_process\").execSync(`devctl rollout wait inst o/r --pr ${1}`)'", "a devctl rollout wait in node's code (-e)"},
		{"bash -c 'devctl pr merge o/r 7 --rebase'", "inside a shell's -c or eval string"},
	} {
		d := decide(t, h, dir, c.cmd, nil)
		if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, c.want) {
			t.Errorf("%q: want a refusal naming %q, got %+v", c.cmd, c.want, d)
		}
	}
	// A list a program runs through the gate is no ungated merge.
	if d := decide(t, h, dir, "python3 - <<'EOF'\nimport subprocess\nsubprocess.run(['"+self+"', 'gate', '--', 'devctl', 'pr', 'merge', 'o/r', '7'])\nEOF", nil); d != nil && d.PermissionDecision == decisionDeny {
		t.Errorf("a gated list is refused: %s", d.Reason)
	}
	// A shell command line written in a here-document stays gated in place.
	if d := decide(t, h, dir, "bash <<'EOF'\ndevctl pr merge o/r 7 --rebase\nEOF", nil); d == nil || d.UpdatedInput["command"] != "bash <<'EOF'\n"+g+"devctl pr merge o/r 7 --rebase\nEOF" {
		t.Errorf("a shell's here-document: got %+v", d)
	}
}
