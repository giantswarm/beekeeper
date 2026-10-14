//go:build unix

package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
)

// baseRedDoc is devctl pr merge's document of a pull request red on the
// non-required e2e-test alone.
const baseRedDoc = `{"exitCode":1,"verdict":"red","reason":"check e2e-test concluded failure","baseRef":"main","mergeCommitSha":"",` +
	`"checks":[{"name":"e2e-test","conclusion":"failure","required":false},{"name":"go-build","conclusion":"success","required":true}],"release":null}`

// acceptingDevctl puts a devctl on PATH that is red (baseRedDoc) unless it
// runs with --accept-red, merging then; it returns the file its argument
// vectors go to, one line per run.
func acceptingDevctl(t *testing.T) string {
	t.Helper()
	calls := filepath.Join(t.TempDir(), "calls")
	fakeDevctl(t, `echo "$*" >>`+calls+`
case "$*" in *--accept-red*) echo '`+mergedDoc+`';; *) echo '`+baseRedDoc+`'; exit 1;; esac`)
	return calls
}

// stubBase answers the base's checks with e2e-test's outcome on its head
// and whether the installed devctl has --accept-red.
func stubBase(t *testing.T, e2e string, accepts bool) {
	t.Helper()
	baseChecks = func(context.Context, string, string) (github.BaseChecks, error) {
		return github.BaseChecks{Base: mainBranch, Head: "0123456789", Required: []string{"go-build"},
			Outcomes: map[string]string{"e2e-test": e2e, "go-build": github.CheckPass}}, nil
	}
	devctlAcceptsRed = func(context.Context) bool { return accepts }
}

func devctlCalls(t *testing.T, path string) []string {
	t.Helper()
	raw, _ := os.ReadFile(path) //nolint:gosec // the test's file
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// A pull request red only on a non-required check that is red on the base
// head too merges: devctl runs again accepting that check, the caller reads
// the merge's document alone, and the event names the check and why.
func TestARedCheckBrokenOnTheBaseIsLetThrough(t *testing.T) {
	noSystemd(t)
	calls := acceptingDevctl(t)
	stubGitHub(t, github.Open, "")
	stubBase(t, github.CheckFail, true)
	stdout := gateStdout(t)
	g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}})
	if err := g.runMerge(); err != nil {
		t.Fatalf("the merge red on a base-red check: %v", err)
	}
	got := devctlCalls(t, calls)
	if len(got) != 2 || !strings.Contains(got[1], "--accept-red e2e-test") || strings.Contains(got[0], "--accept-red") {
		t.Errorf("devctl ran %q, want once plain and once with --accept-red e2e-test", got)
	}
	if out := stdout(); out != mergedDoc {
		t.Errorf("the caller read %q, want the merge's document alone", out)
	}
	if d := lastEvent(t, g, "merge.acceptred"); !strings.Contains(d, "e2e-test let through: not required, and red on the head 0123456 of main too") {
		t.Errorf("merge.acceptred event: %q", d)
	}
	if d := lastEvent(t, g, "merged"); !strings.Contains(d, "o/r#7 exit 0, release v1.2.4") {
		t.Errorf("merged event: %q", d)
	}
}

// A red check green on the base head, or any red check while the installed
// devctl has no --accept-red, ends the merge red as before: devctl runs
// once, and the caller reads its red document.
func TestARedCheckTheGateDoesNotLetThroughRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		e2e     string
		accepts bool
	}{
		"green on the base head":  {github.CheckPass, true},
		"no result on the base":   {github.CheckMissing, true},
		"devctl without the flag": {github.CheckFail, false},
	} {
		t.Run(name, func(t *testing.T) {
			noSystemd(t)
			calls := acceptingDevctl(t)
			stubGitHub(t, github.Open, "")
			stubBase(t, tc.e2e, tc.accepts)
			stdout := gateStdout(t)
			g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}})
			if err := g.runMerge(); Code(err) != 1 {
				t.Fatalf("exit %d, want devctl's 1", Code(err))
			}
			if got := devctlCalls(t, calls); len(got) != 1 {
				t.Errorf("devctl ran %q, want once", got)
			}
			if out := stdout(); out != baseRedDoc {
				t.Errorf("the caller read %q, want the red document", out)
			}
			if st := gateState(t, g); len(st.Failed) != 1 {
				t.Errorf("failed attempts %+v, want the red run", st.Failed)
			}
		})
	}
}

// A red check whose job died on a transient infrastructure failure is not
// the pull request's break: the gate refuses it as infra, and the failed
// attempt's reason, which wakes its owner, names the matched line and the
// rerun of that job.
func TestARedCheckOnAnInfraFlakeIsNotThePullRequestsBreak(t *testing.T) {
	noSystemd(t)
	calls := acceptingDevctl(t)
	stubGitHub(t, github.Open, "")
	stubBase(t, github.CheckPass, true)
	infraReds = func(context.Context, []byte) []merge.Infra {
		return []merge.Infra{{Check: "e2e-test", Repo: scratchRepo, Run: 11, Job: 12, Signature: "Go module proxy",
			Line: `read "https://proxy.golang.org/golang.org/x/text/@v/v0.41.0.zip": stream error: stream ID 143; INTERNAL_ERROR; received from peer`}}
	}
	_ = gateStdout(t)
	g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}})
	if err := g.runMerge(); Code(err) != 1 {
		t.Fatalf("exit %d, want devctl's 1", Code(err))
	}
	if got := devctlCalls(t, calls); len(got) != 1 {
		t.Errorf("devctl ran %q, want once: an infra flake is never let through", got)
	}
	st := gateState(t, g)
	if len(st.Failed) != 1 {
		t.Fatalf("failed attempts %+v, want the red run", st.Failed)
	}
	why := st.Failed[0].Reason
	for _, want := range []string{"e2e-test: a transient infrastructure failure, not this pull request's breakage (Go module proxy: ",
		"stream error: stream ID 143", "rerun the job (`gh run rerun 11 --job 12 --repo o/r`)"} {
		if !strings.Contains(why, want) {
			t.Errorf("failed reason %q, want it to carry %q", why, want)
		}
	}
}
