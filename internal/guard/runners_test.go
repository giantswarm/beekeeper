package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/lease"
)

// The runners the refusals name.
const (
	xargsRunner     = "xargs"
	parallelRunner  = "parallel"
	backquoteRunner = "a backquote substitution"
)

// TestHookRefusesRunnerMerges: a merge, promote or wait that a runner runs
// from its arguments (xargs, find -exec, parallel, sudo, watch) is at no
// command position the gate rewrite reaches: the call is refused, naming it
// with the gate written in after the runner, and that form passes.
func TestHookRefusesRunnerMerges(t *testing.T) {
	h := Hook{Self: self, Shell: testShell, Clusters: func() []string { return nil }, Leases: func() []lease.Holder { return nil }}
	g := self + " gate -- "
	for _, c := range []struct {
		cmd, runner, fixed string
		bg                 bool
	}{
		{`printf '%s\n' 7 8 | xargs -n1 devctl pr merge o/r`, xargsRunner, `printf '%s\n' 7 8 | xargs -n1 ` + g + `devctl pr merge o/r`, false},
		{`gh pr list --repo o/r --json number -q '.[].number' | xargs -I{} devctl pr merge o/r {} --update-branch`, xargsRunner,
			`gh pr list --repo o/r --json number -q '.[].number' | xargs -I{} ` + g + `devctl pr merge o/r {} --update-branch`, false},
		{`xargs -a prs.txt -n1 ~/bin/devctl pr wait o/r`, xargsRunner, `xargs -a prs.txt -n1 ` + g + `~/bin/devctl pr wait o/r`, false},
		{`find . -name '*.pr' -exec devctl pr merge o/r {} \;`, "find -exec", `find . -name '*.pr' -exec ` + g + `devctl pr merge o/r {} \;`, false},
		{`find prs -type f -execdir devctl release promote o/r \;`, "find -execdir", `find prs -type f -execdir ` + g + `devctl release promote o/r \;`, false},
		{`parallel devctl pr merge o/r ::: 7 8`, parallelRunner, `parallel ` + g + `devctl pr merge o/r ::: 7 8`, false},
		{`parallel -j2 'devctl pr merge o/r {}' ::: 7 8`, parallelRunner, `parallel -j2 '` + g + `devctl pr merge o/r {}' ::: 7 8`, false},
		{`seq 7 8 | parallel devctl pr merge o/r`, parallelRunner, `seq 7 8 | parallel ` + self + ` gate --wait 30m --limit 0 -- devctl pr merge o/r`, true},
		{`sudo -E devctl pr merge o/r 7`, "sudo", `sudo -E ` + g + `devctl pr merge o/r 7`, false},
		{`watch -n 60 devctl pr wait o/r 7`, "watch", `watch -n 60 ` + g + `devctl pr wait o/r 7`, false},
		{`cd x && nice -n 5 xargs devctl rollout wait inst o/r --pr < prs`, xargsRunner, `cd x && nice -n 5 xargs ` + g + `devctl rollout wait inst o/r --pr < prs`, false},
		// Unquoted, it fails closed whatever runs it: an unlisted wrapper, a quoted runner name, a multi-call binary.
		{`busybox xargs devctl pr merge o/r < prs`, xargsRunner, `busybox xargs ` + g + `devctl pr merge o/r < prs`, false},
		{`"xargs" -n1 devctl pr merge o/r < prs`, "`\"xargs\"`", `"xargs" -n1 ` + g + `devctl pr merge o/r < prs`, false},
		{`time -v devctl pr merge o/r 7`, "`time`", `time -v ` + g + `devctl pr merge o/r 7`, false},
		// A shell's -c named only in a quoted argument is no -c string.
		{`wrap --note 'sh -c' devctl pr merge o/r 7`, "`wrap`", `wrap --note 'sh -c' ` + g + `devctl pr merge o/r 7`, false},
		// A here-document a shell reads is a command line: the runner check reads it too.
		{"bash <<'EOF'\nxargs -n1 devctl pr merge o/r < prs\nEOF", xargsRunner, "xargs -n1 " + g + "devctl pr merge o/r < prs\nEOF", false},
		{"bash <<'EOF'\nfind . -exec devctl pr merge o/r {} +\nEOF", "find -exec", "find . -exec " + g + "devctl pr merge o/r {} +\nEOF", false},
		{"cat <<'EOF' | sh\nparallel devctl pr merge o/r ::: 7 8\nEOF", parallelRunner, "parallel " + g + "devctl pr merge o/r ::: 7 8\nEOF", false},
		{"bash <<'EOF'\nsudo devctl pr merge o/r 7\nEOF", "sudo", "sudo " + g + "devctl pr merge o/r 7\nEOF", false},
		// A backquote substitution is a command the gate rewrite does not reach either.
		{"x=`devctl pr wait o/r 7`", backquoteRunner, "x=`" + g + "devctl pr wait o/r 7`", false},
		{"echo \"done: ` devctl pr merge o/r 7`\"", backquoteRunner, "echo \"done: ` " + g + "devctl pr merge o/r 7`\"", false},
	} {
		d := decide(t, h, t.TempDir(), c.cmd, map[string]any{backgroundKey: c.bg})
		if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "Refused: "+c.runner+" runs "+ownedWhat) ||
			!strings.HasSuffix(d.Reason, "\n"+c.fixed) {
			t.Errorf("%q: got %+v, want a refusal naming %s and %q", c.cmd, d, c.runner, c.fixed)
			continue
		}
		if d := decide(t, h, t.TempDir(), c.fixed, map[string]any{backgroundKey: c.bg}); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("%q: the gated form is refused too: %s", c.fixed, d.Reason)
		}
	}
	// Text that names a runner and a merge runs none; a runner without a
	// merge, or with a devctl read, passes.
	for _, cmd := range []string{
		`echo xargs devctl pr merge o/r 7`,
		`gh pr comment 7 --repo o/r --body "never: xargs devctl pr merge o/r"`,
		`git commit -m 'refuse find -exec devctl pr merge'`,
		`grep -n 'xargs devctl pr merge' log.txt`,
		`seq 1 3 | xargs -n1 devctl pr view o/r`,
		`find . -name '*.go' -exec grep -l 'devctl pr merge' {} +`,
		`xargs -n1 echo < prs.txt; devctl pr view o/r 7`,
		`xargs echo devctl pr merge o/r < prs`,
		// A beekeeper note, park or report is text for a person.
		`beekeeper note add devctl pr merge o/r 7 needs a look`,
		`beekeeper agents idle --done --report merged with devctl pr merge o/r 7`,
		"cat <<'EOF' >| notes.md\nxargs devctl pr merge o/r\nEOF",
		// An escaped backquote is text.
		"gh pr comment 7 --repo o/r --body \"ran \\`devctl pr merge o/r 7\\` by hand\"",
	} {
		if d := decide(t, h, t.TempDir(), cmd, nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("%q is refused: %s", cmd, d.Reason)
		}
	}
}

// TestHookRefusesRunnerMergesInScripts: a runner's merge in a script the call
// runs or in a here-string a shell reads is refused like one on the command
// line, the refusal naming the script and the line; a merge a remote host runs
// (ssh) is refused with no gated form, since this binary's path names no gate
// there.
func TestHookRefusesRunnerMergesInScripts(t *testing.T) {
	h := Hook{Self: self, Shell: testShell, Clusters: func() []string { return nil }, Leases: func() []lease.Holder { return nil }}
	dir := t.TempDir()
	var exe os.FileMode = 0o700 // ./f.sh runs by its path
	for name, body := range map[string]string{
		"x.sh": "#!/usr/bin/env bash\nset -e\nxargs -n1 devctl pr merge o/r < prs\n",
		"f.sh": "#!/usr/bin/env bash\nfind . -name '*.pr' -exec devctl pr merge o/r {} +\n",
		"p.sh": "#!/usr/bin/env bash\nparallel devctl pr merge o/r ::: 7 8\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), exe); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ cmd, want string }{
		{"bash x.sh", "the script " + filepath.Join(dir, "x.sh") + " runs outside the gate (line 3: xargs -n1 devctl pr merge o/r < prs)"},
		{"./f.sh", "the script " + filepath.Join(dir, "f.sh") + " runs outside the gate (line 2: find . -name '*.pr' -exec devctl pr merge o/r {} +)"},
		{"sh p.sh", "the script " + filepath.Join(dir, "p.sh") + " runs outside the gate (line 2: parallel devctl pr merge o/r ::: 7 8)"},
		{"sh <<< 'xargs devctl pr merge o/r < prs'", "the here-string sh reads runs outside the gate"},
	} {
		d := decide(t, h, dir, c.cmd, nil)
		if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, c.want) {
			t.Errorf("%q: got %+v, want a refusal naming %q", c.cmd, d, c.want)
		}
	}
	d := decide(t, h, dir, "ssh host 'devctl pr merge o/r 7'", nil)
	if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "ssh runs "+ownedWhat) || strings.Contains(d.Reason, " gate -- ") {
		t.Errorf("ssh: got %+v, want a refusal with no gated form", d)
	}
}
