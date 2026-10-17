//go:build unix

package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

// classicDeclineDoc is devctl pr merge's document of a merge GitHub declined
// on classic branch protection, its reason as devctl writes it.
const classicDeclineDoc = `{"exitCode":3,"verdict":"not_applicable","repository":"o/r","number":7,"baseRef":"main","mergeCommitSha":"","release":null,` +
	`"reason":"merge declined error: GitHub declined the merge of o/r#7: At least 1 approving review is required by reviewers with write access. ` +
	`devctl acts as pat, who has no bypass on the review rule of main, which no ruleset carries: classic branch protection requires the review, ` +
	`and such a repository is aligned first, never merged past it: align: true on the entry r in repositories/team-o.yaml of giantswarm/github@main ` +
	`gives it the devctl ruleset, whose bypass lets the owning team (team-o) and the repository admins merge their own green pull requests; ` +
	`devctl never lifts enforce_admins, and its token carries no Administration permission to do so"}`

// A merge declined on classic protection ends with one gate line naming the
// cause and the alignment, whole past the reason's truncation; devctl runs
// once.
func TestAClassicProtectionDeclineNamesTheAlignment(t *testing.T) {
	noSystemd(t)
	calls := filepath.Join(t.TempDir(), "calls")
	fakeDevctl(t, `echo "$*" >>`+calls+`; echo '`+classicDeclineDoc+`'; exit 3`)
	stubGitHub(t, "", "")
	g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}})
	var err error
	out := gateStderr(t, func() { err = g.runMerge() })
	var code *exitError
	if !errors.As(err, &code) || code.code != 3 {
		t.Errorf("exit %v, want 3", err)
	}
	want := "classic branch protection of o/r requires a review the merge has no bypass for, and enforce_admins does not lift for it " +
		"(no token of ours may change it: HTTP 403): align the repository, align: true on the entry r in repositories/team-o.yaml of giantswarm/github (devctl repo status o/r)"
	lines := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, GatePrefix+"nothing merged") {
			lines++
			if !strings.Contains(l, want) {
				t.Errorf("the gate line %q does not name the cause and the fix %q", l, want)
			}
		}
	}
	if lines != 1 {
		t.Errorf("%d nothing-merged gate lines, want one:\n%s", lines, out)
	}
	if raw, _ := os.ReadFile(calls); strings.Count(string(raw), "\n") != 1 { //nolint:gosec // the test's file
		t.Errorf("devctl ran %q, want once", raw)
	}
}
