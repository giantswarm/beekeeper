//go:build unix

package cmd

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// redDoc is devctl pr merge's document of a pull request red on two checks
// and a CircleCI workflow, the reason naming one of them.
const redDoc = `{"exitCode":1,"verdict":"red","reason":"status ci/circleci: go-build is failure","mergeCommitSha":"",` +
	`"checks":[{"name":"ci/circleci: go-build","conclusion":"failure"},{"name":"lint / golangci","conclusion":"failure"},{"name":"test","conclusion":"success"}],` +
	`"circleci":{"workflows":[{"name":"build","status":"failing"},{"name":"setup","status":"success"}]},"release":null}`

const redReason = "red: status ci/circleci: go-build is failure; failed: lint / golangci, circleci workflow build"

// A gated merge that ends red leaves no heard marker, so its merge-child
// wakes the owner with devctl's verdict and the failed checks, as for a
// queued merge's outcome; the lane shows the failed attempt with its reason.
func TestARedMergeWakesItsOwnerAndStaysOnItsLane(t *testing.T) {
	noSystemd(t)
	fakeDevctl(t, `echo "waiting for CI" >&2; echo '`+redDoc+`'; exit 1`)
	stubGitHub(t, "", "")
	lane := config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}}
	g := runningMerge(t, scratchRepo, lane)
	owner := state.Party{Session: "s1", Name: ownerName}
	g.me = owner
	if err := g.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges[0].By = owner
		st.Agents = []state.Agent{{Party: owner}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.runMerge(); Code(err) != 1 {
		t.Fatalf("exit %d, want devctl's 1", Code(err))
	}
	base := mergeBase(g.store.Dir(), g.repo, g.pr)
	if _, err := os.Stat(heardFile(base, os.Getpid())); err == nil {
		t.Fatal("a red merge left the heard marker: its merge-child wakes nobody")
	}

	st := gateState(t, g)
	if len(st.Merges) != 0 {
		t.Errorf("a red merge keeps a place: %+v", st.Merges)
	}
	if len(st.Failed) != 1 || st.Failed[0].Key() != scratchRepo+"#7" || st.Failed[0].Exit != 1 || st.Failed[0].Reason != redReason ||
		st.Failed[0].By.Name != ownerName || st.Failed[0].Lane != scratchRepo {
		t.Errorf("failed attempts %+v", st.Failed)
	}
	if d := lastEvent(t, g, "merge.failed"); !strings.Contains(d, "o/r#7 exit 1, nothing merged, it left lane "+scratchRepo+": "+redReason) {
		t.Errorf("merge.failed event: %q", d)
	}

	// merge-child hands the outcome over once the gate is gone (tellOwner).
	var woke []string
	was := wakeOwner
	wakeOwner = func(_ *app, _ context.Context, by state.Party, q, msg, _ string) error {
		woke = append(woke, by.Name+" → "+q+": "+msg)
		return nil
	}
	t.Cleanup(func() { wakeOwner = was })
	g.tellOwner(context.Background(), childResult{spec: childSpec{Argv: g.argv, Owner: owner}, base: base, rc: 1, doc: []byte(redDoc)})
	want := "beekeeper gate → s1: devctl pr merge o/r 7 exit 1: " + redReason
	if len(woke) != 1 || woke[0] != want {
		t.Errorf("woke %q, want %q", woke, want)
	}

	var out bytes.Buffer
	g.out = &out
	g.printLanes(g.laneViews(st))
	if want := `failed o/r#7 by "worker" at`; !strings.Contains(out.String(), want) ||
		!strings.Contains(out.String(), "nothing merged, exit 1: "+redReason+"; shown until its next attempt") {
		t.Errorf("lanes:\n%s", out.String())
	}
}

// A merge that ends green leaves the heard marker for a caller still
// listening, and drops the pull request's earlier failed attempt.
func TestAGreenMergeIsHeardAndLeavesNoFailedAttempt(t *testing.T) {
	noSystemd(t)
	fakeDevctl(t, `echo '`+mergedDoc+`'`)
	stubGitHub(t, "", "")
	g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}})
	if err := g.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Failed = []state.Failed{{Repo: scratchRepo, PR: 7, Lane: scratchRepo, Exit: 1, Reason: "red"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.runMerge(); err != nil {
		t.Fatalf("exit %d: %v", Code(err), err)
	}
	base := mergeBase(g.store.Dir(), g.repo, g.pr)
	if _, err := os.Stat(heardFile(base, os.Getpid())); err != nil {
		t.Errorf("no heard marker: %v", err)
	}
	_ = os.Remove(heardFile(base, os.Getpid()))
	if st := gateState(t, g); len(st.Failed) != 0 {
		t.Errorf("failed attempts %+v", st.Failed)
	}
}
