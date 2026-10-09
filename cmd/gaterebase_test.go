//go:build unix

package cmd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

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
			err := a.gate(context.Background(), mergeArgv(scratchRepo, "--rebase"), 0, false)
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
			if err := a.gate(context.Background(), mergeArgv(scratchRepo, "--rebase"), 0, false); Code(err) != ExitGateQueued {
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
	if err := a.gate(context.Background(), mergeArgv(scratchRepo, "--rebase"), 0, false); Code(err) != ExitGateRefused {
		t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitGateRefused)
	}
	if d := lastEventOf(t, a, "merge.refused"); !strings.Contains(d, "connection reset") || placeOf(t, a) >= 0 {
		t.Errorf("refusal %q, place %d", d, placeOf(t, a))
	}

	a = busyLaneApp(t)
	if err := a.gate(context.Background(), mergeArgv(scratchRepo), 0, false); Code(err) != ExitGateQueued {
		t.Fatalf("the squash merge: exit %d (%v), want %d", Code(err), err, ExitGateQueued)
	}
	a = busyLaneApp(t)
	if err := a.gate(context.Background(), mergeArgv(scratchRepo, "--rebase"), 0, true); Code(err) != ExitGateRefused ||
		strings.Contains(lastEventOf(t, a, "merge.refused"), "rebase") {
		t.Fatalf("the queued run: exit %d (%v), want the wait's own refusal", Code(err), err)
	}
	if *asked != 1 {
		t.Errorf("GitHub was read %d times, want once", *asked)
	}
}
