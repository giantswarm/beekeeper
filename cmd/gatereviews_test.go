//go:build unix

package cmd

import (
	"context"
	"strings"
	"syscall"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// changesRequested is the reason github.ReviewGuard gives for a standing
// request for changes.
const changesRequested = "changes requested: review CHANGES_REQUESTED by marians https://github.com/o/r/pull/7#r; a new commit or a reply leaves it standing, only the reviewer's approval or the review's dismissal clears it"

// stubReviews answers reviewGuard with why and err and counts the reads.
func stubReviews(t *testing.T, why string, err error) *int {
	t.Helper()
	asked := new(int)
	was := reviewGuard
	t.Cleanup(func() { reviewGuard = was })
	reviewGuard = func(context.Context, string, int) (string, error) {
		*asked++
		return why, err
	}
	return asked
}

// A fresh merge a reviewer holds is refused (77) before it takes a lane
// place, naming the reviewer; one whose reviews cannot be read in transit
// is unread (74): nothing merges unread.
func TestAReviewerHoldsAFreshMerge(t *testing.T) {
	for name, c := range map[string]struct {
		why  string
		err  error
		code int
		says string
	}{
		"changes requested": {why: changesRequested, code: ExitGateRefused, says: "o/r#7 waits on its reviewers, nothing merged: changes requested: review CHANGES_REQUESTED by marians"},
		"in transit":        {err: syscall.ECONNRESET, code: ExitGateUnread, says: "reading the reviews of o/r#7 failed in transit"},
		"unreadable":        {err: errFake("HTTP 404"), code: ExitGateRefused, says: "the reviews of o/r#7 cannot be read"},
	} {
		t.Run(name, func(t *testing.T) {
			stubGitHub(t, github.Open, "")
			stubReviews(t, c.why, c.err)
			a := busyLaneApp(t)
			err := a.gate(context.Background(), mergeArgv(scratchRepo), 0, 0, false)
			if Code(err) != c.code {
				t.Fatalf("exit %d (%v), want %d", Code(err), err, c.code)
			}
			if i := placeOf(t, a); i >= 0 {
				t.Errorf("the refused merge took a lane place")
			}
			if d := lastEventOf(t, a, "merge.refused"); !strings.Contains(d, c.says) {
				t.Errorf("refusal %q, want %q", d, c.says)
			}
		})
	}
}

// A promotion has no reviews to read.
func TestAPromotionReadsNoReviews(t *testing.T) {
	asked := stubReviews(t, changesRequested, nil)
	g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo})
	g.pr = 0
	if err := g.checkReviews(g.refuseWith); err != nil || *asked != 0 {
		t.Errorf("%v, %d reads", err, *asked)
	}
}

// Feedback that landed while the merge waited refuses it at its turn: its
// running place leaves the lane, devctl never runs.
func TestAReviewerHoldsAMergeAtItsTurn(t *testing.T) {
	stubGitHub(t, github.Open, "")
	stubReviews(t, changesRequested, nil)
	g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo})
	err := g.checkReviews(g.refuseRunningWith)
	if Code(err) != ExitGateRefused {
		t.Fatalf("exit %d (%v)", Code(err), err)
	}
	st, err := g.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range st.Merges {
		if m.PR == 7 && m.Phase == state.Running {
			t.Errorf("the refused merge keeps its running place")
		}
	}
	if d := lastEvent(t, g, "merged"); d != "" {
		t.Errorf("merged: %q", d)
	}
}

// The gate's dry path judges the reviews alone: 77 naming the reviewer, 74
// in transit, 0 when they let the merge through; nothing reaches the state.
func TestGateCheckIsTheDryPath(t *testing.T) {
	for name, c := range map[string]struct {
		why  string
		err  error
		code int
		says string
	}{
		"changes requested": {why: changesRequested, code: ExitGateRefused, says: "refused, o/r#7 waits on its reviewers, nothing merged: changes requested: review CHANGES_REQUESTED by marians"},
		"in transit":        {err: syscall.ECONNRESET, code: ExitGateUnread, says: "unread, reading the reviews of o/r#7 failed in transit"},
		"nothing holds":     {says: "dry path: o/r#7 would merge as far as its reviews go"},
	} {
		t.Run(name, func(t *testing.T) {
			stubReviews(t, c.why, c.err)
			a := busyLaneApp(t)
			var err error
			out := gateStderr(t, func() { err = a.gateCheck(context.Background(), scratchRepo, 7) })
			if c.code == 0 && err != nil || c.code != 0 && Code(err) != c.code {
				t.Fatalf("exit %d (%v), want %d", Code(err), err, c.code)
			}
			if !strings.Contains(out, c.says) {
				t.Errorf("%q lacks %q", out, c.says)
			}
			if d := lastEventOf(t, a, "merge.refused"); d != "" {
				t.Errorf("the dry path logged %q", d)
			}
		})
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }
