//go:build unix

package cmd

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// stubPullRebase answers pullRebase with r and err and counts the reads.
func stubPullRebase(t *testing.T, r github.Rebase, err error) *int {
	t.Helper()
	asked := new(int)
	was, retry := pullRebase, rebaseRetry
	t.Cleanup(func() { pullRebase, rebaseRetry = was, retry })
	rebaseRetry = 0
	pullRebase = func(context.Context, string, int) (github.Rebase, error) {
		*asked++
		return r, err
	}
	return asked
}

// placeOf is the index of o/r#7's place in a's lane, -1 when it has none.
func placeOf(t *testing.T, a *app) int {
	t.Helper()
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return slices.IndexFunc(st.Merges, func(m state.Merge) bool { return m.PR == 7 })
}

// A rebase merge GitHub reports not rebaseable, or conflicting with its base
// though a base merge made the branch itself clean, is refused before it
// takes a lane place, naming the replacement PR as the way out.
func TestANonRebaseableMergeIsRefusedBeforeItQueues(t *testing.T) {
	for name, r := range map[string]github.Rebase{
		"not rebaseable": {Known: true, State: "clean"},
		"dirty":          {Known: true, Rebaseable: true, State: github.DirtyState},
	} {
		t.Run(name, func(t *testing.T) {
			stubGitHub(t, github.Open, "")
			asked := stubPullRebase(t, r, nil)
			a := busyLaneApp(t)
			err := a.gate(context.Background(), mergeArgv(scratchRepo, "--rebase"), 0, 0, false)
			if Code(err) != ExitGateRefused {
				t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitGateRefused)
			}
			if *asked != 1 {
				t.Errorf("GitHub was read %d times, want once", *asked)
			}
			if i := placeOf(t, a); i >= 0 {
				t.Errorf("the refused merge took a lane place")
			}
			if d := lastEventOf(t, a, "merge.queued"); d != "" {
				t.Errorf("the refused merge queued: %q", d)
			}
			if d := lastEventOf(t, a, "merge.refused"); !strings.Contains(d, "o/r#7 cannot be rebased onto its base") || !strings.Contains(d, "replacement PR") {
				t.Errorf("refusal %q", d)
			}
		})
	}
}

// A rebaseable merge queues as before; so does one whose rebase GitHub has
// not computed after its reads, devctl judging it at its turn.
func TestARebaseableMergeQueuesAsBefore(t *testing.T) {
	for name, c := range map[string]struct {
		r     github.Rebase
		reads int
	}{
		"rebaseable":   {github.Rebase{Known: true, Rebaseable: true, State: "blocked"}, 1},
		"not computed": {github.Rebase{State: "unknown"}, rebaseReads},
	} {
		t.Run(name, func(t *testing.T) {
			stubGitHub(t, github.Open, "")
			stubSelf(t)
			asked := stubPullRebase(t, c.r, nil)
			a := busyLaneApp(t)
			if err := a.gate(context.Background(), mergeArgv(scratchRepo, "--rebase"), 0, 0, false); Code(err) != ExitGateQueued {
				t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitGateQueued)
			}
			if *asked != c.reads {
				t.Errorf("GitHub was read %d times, want %d", *asked, c.reads)
			}
			if placeOf(t, a) < 0 {
				t.Errorf("the merge has no lane place")
			}
		})
	}
}

// GitHub not answering whether the pull request rebases refuses it before
// it queues; a squash merge and a queued run, which has its place already,
// are not asked.
func TestTheRebaseReadOnlyGuardsAFreshRebaseMerge(t *testing.T) {
	stubGitHub(t, github.Open, "")
	stubSelf(t)
	asked := stubPullRebase(t, github.Rebase{}, errors.New("connection reset"))
	a := busyLaneApp(t)
	if err := a.gate(context.Background(), mergeArgv(scratchRepo, "--rebase"), 0, 0, false); Code(err) != ExitGateRefused {
		t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitGateRefused)
	}
	if d := lastEventOf(t, a, "merge.refused"); !strings.Contains(d, "connection reset") || placeOf(t, a) >= 0 {
		t.Errorf("refusal %q, place %d", d, placeOf(t, a))
	}

	a = busyLaneApp(t)
	if err := a.gate(context.Background(), mergeArgv(scratchRepo), 0, 0, false); Code(err) != ExitGateQueued {
		t.Fatalf("the squash merge: exit %d (%v), want %d", Code(err), err, ExitGateQueued)
	}
	a = busyLaneApp(t)
	if err := a.gate(context.Background(), mergeArgv(scratchRepo, "--rebase"), 0, 0, true); Code(err) != ExitGateRefused ||
		strings.Contains(lastEventOf(t, a, "merge.refused"), "rebase") {
		t.Fatalf("the queued run: exit %d (%v), want the wait's own refusal", Code(err), err)
	}
	if *asked != 1 {
		t.Errorf("GitHub was read %d times, want once", *asked)
	}
}

// rebaseTurn stubs a rebase merge's turn: GitHub answers the reads with
// reads in order (the last one repeated), the rebase update with updateErr,
// and devctl merges, its arguments written to the returned file. It returns
// the gate run, the reads and updates asked, and devctl's arguments file.
func rebaseTurn(t *testing.T, updateErr error, reads ...github.Rebase) (g *gateRun, asked, updates *int, args string) {
	t.Helper()
	noSystemd(t)
	args = t.TempDir() + "/args"
	fakeDevctl(t, `echo "$*" > `+args+`; echo '`+mergedDoc+`'`)
	stubGitHub(t, github.Merged, "")
	stubBaseRelease(t, github.BaseRelease{Base: mainBranch, Auto: true}, nil)
	asked, updates = new(int), new(int)
	was, update, retry := pullRebase, updateRebase, rebaseRetry
	t.Cleanup(func() { pullRebase, updateRebase, rebaseRetry = was, update, retry })
	rebaseRetry = 0
	pullRebase = func(context.Context, string, int) (github.Rebase, error) {
		r := reads[min(*asked, len(reads)-1)]
		*asked++
		return r, nil
	}
	updateRebase = func(_ context.Context, repo string, n int) error {
		if repo != scratchRepo || n != 7 {
			t.Errorf("rebased %s#%d", repo, n)
		}
		*updates++
		return updateErr
	}
	g = runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}})
	g.argv = mergeArgv(scratchRepo, "--rebase")
	return g, asked, updates, args
}

// A rebase merge that waited in its lane behind the merge that just landed
// is only behind its base at its turn: GitHub rebases it onto the base, the
// gate awaits the new head and devctl, unchanged, merges it.
func TestARebaseMergeBehindItsBaseAtItsTurnIsRebasedAndMerged(t *testing.T) {
	behind := github.Rebase{Known: true, Rebaseable: true, State: github.BehindState, Head: "aaaaaaaaaaaa"}
	stale := github.Rebase{Known: true, Rebaseable: true, State: github.BehindState, Head: "aaaaaaaaaaaa"}
	rebased := github.Rebase{Known: true, Rebaseable: true, State: "blocked", Head: "bbbbbbbbbbbb"}
	g, asked, updates, args := rebaseTurn(t, nil, behind, stale, rebased)
	if err := startMerge(t, g); err != nil {
		t.Fatalf("exit %d (%v), want 0", Code(err), err)
	}
	if *updates != 1 || *asked != 3 {
		t.Errorf("%d rebase updates, %d reads; want one update, then reads until the new head", *updates, *asked)
	}
	if d := lastEvent(t, g, "merge.rebased"); !strings.Contains(d, "o/r#7 was behind its base at its turn") || !strings.Contains(d, "(head bbbbbbbbb)") {
		t.Errorf("merge.rebased event: %q", d)
	}
	if got, err := os.ReadFile(args); err != nil || !strings.HasPrefix(string(got), "pr merge o/r 7 --rebase") { //nolint:gosec // the test's own file
		t.Errorf("devctl ran with %q (%v), want the gated command", got, err)
	}
	if d := lastEvent(t, g, verbMerged); !strings.Contains(d, "o/r#7 exit 0") {
		t.Errorf("merged event: %q", d)
	}
}

// A rebase merge that conflicts with its base at its turn, or that GitHub
// does not rebase, is refused with nothing merged: devctl does not run, the
// place leaves the lane and the refusal names the replacement PR.
func TestARebaseMergeThatCannotCatchUpAtItsTurnIsRefused(t *testing.T) {
	for name, c := range map[string]struct {
		r         github.Rebase
		updateErr error
		updates   int
		why       string
	}{
		"conflicts": {github.Rebase{Known: true, Rebaseable: true, State: github.DirtyState}, nil, 0, "conflicts with its base"},
		"not rebased": {github.Rebase{Known: true, Rebaseable: true, State: github.BehindState}, errors.New("merge conflict between base and head"), 1,
			"which GitHub does not rebase it onto (merge conflict between base and head)"},
	} {
		t.Run(name, func(t *testing.T) {
			g, _, updates, args := rebaseTurn(t, c.updateErr, c.r)
			if err := startMerge(t, g); Code(err) != ExitGateRefused {
				t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitGateRefused)
			}
			if *updates != c.updates {
				t.Errorf("%d rebase updates, want %d", *updates, c.updates)
			}
			if _, err := os.Stat(args); err == nil {
				t.Errorf("devctl ran")
			}
			if st := gateState(t, g); len(st.Merges) != 0 {
				t.Errorf("the place stays in the lane: %+v", st.Merges)
			}
			if d := lastEvent(t, g, "merge.refused"); !strings.Contains(d, "its turn came") || !strings.Contains(d, c.why) ||
				!strings.Contains(d, "nothing merged") || !strings.Contains(d, "replacement PR") {
				t.Errorf("refusal %q", d)
			}
		})
	}
}

// A rebase merge up to date with its base, or one GitHub does not answer
// for at its turn, runs devctl as it stands; a squash merge is not read.
func TestARebaseMergeUpToDateAtItsTurnRunsAsItStands(t *testing.T) {
	for name, squash := range map[string]bool{"rebase, up to date": false, "squash": true} {
		t.Run(name, func(t *testing.T) {
			g, asked, updates, args := rebaseTurn(t, nil, github.Rebase{Known: true, Rebaseable: true, State: "clean", Head: "abc"})
			if squash {
				g.argv = mergeArgv(scratchRepo)
			}
			if err := startMerge(t, g); err != nil {
				t.Fatalf("exit %d (%v), want 0", Code(err), err)
			}
			want := 1
			if squash {
				want = 0
			}
			if *asked != want || *updates != 0 {
				t.Errorf("%d reads, %d rebase updates; want %d reads, no update", *asked, *updates, want)
			}
			if _, err := os.Stat(args); err != nil {
				t.Errorf("devctl did not run: %v", err)
			}
			if d := lastEvent(t, g, "merge.rebased"); d != "" {
				t.Errorf("merge.rebased event: %q", d)
			}
		})
	}
}
