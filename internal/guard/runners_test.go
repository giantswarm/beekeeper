package guard

import (
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/lease"
)

// TestHookRefusesRunnerMerges: a merge, promote or wait that a runner runs
// from its arguments (xargs, find -exec, parallel, sudo, watch) is at no
// command position the gate rewrite reaches: the call is refused, naming it
// with the gate written in after the runner, and that form passes.
// The runners the refusals name.
const (
	xargsRunner     = "xargs"
	parallelRunner  = "parallel"
	backquoteRunner = "a backquote substitution"
)

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
		{`xargs echo devctl pr merge o/r < prs`, "`xargs`", `xargs echo ` + g + `devctl pr merge o/r < prs`, false},
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
		"cat <<'EOF' >| notes.md\nxargs devctl pr merge o/r\nEOF",
		// An escaped backquote is text.
		"gh pr comment 7 --repo o/r --body \"ran \\`devctl pr merge o/r 7\\` by hand\"",
	} {
		if d := decide(t, h, t.TempDir(), cmd, nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("%q is refused: %s", cmd, d.Reason)
		}
	}
}
