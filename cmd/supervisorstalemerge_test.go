package cmd

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func mergeFailureForTest(head string) string {
	return fmt.Sprintf("merge result (main + %s) failed go test: regression", head)
}

func TestStaleMergeResultFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		stale  bool
	}{
		{"full current", mergeFailureForTest(loopHead), false},
		{"short current", mergeFailureForTest(shortSHA(loopHead)), false},
		{"old head", mergeFailureForTest(shortSHA(loopFixed)), true},
		{"missing identity", "merge result () failed go test", true},
		{"truncated identity", "merge result (main + ", true},
		{"multiple results", mergeFailureForTest(loopHead) + "\n" + mergeFailureForTest(loopFixed), true},
		{"conflict", "branch conflicts with its base", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := staleMergeResultFailure(tc.reason, loopHead); got != tc.stale {
				t.Fatalf("stale = %v, want %v", got, tc.stale)
			}
		})
	}
}

// A surrounding record naming the current head cannot relabel a diagnostic
// from the previous merge result, whether persisted or returned synchronously.
func TestLoopDropsContradictoryMergeFailures(t *testing.T) {
	for _, source := range []string{"stored", "synchronous"} {
		t.Run(source, func(t *testing.T) {
			r := newLoopRig(t)
			r.park(t, loopPR, time.Now())
			r.pass(t)
			r.verdict("R1", loopHead, superloop.Approved, "")
			reason := mergeFailureForTest(shortSHA(loopFixed))
			if source == "stored" {
				if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.Failed = append(st.Failed, state.Failed{Repo: loopMergeRepo, PR: 7, Head: loopHead, By: loopParty, Exit: github.SquashRed, Reason: reason})
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				r.answer = func(args []string) (string, int) {
					if len(args) > 0 && args[0] == gateCmdName {
						return fmt.Sprintf(`{"route":%q,"repository":%q,"number":7,"headSha":%q,"reason":%q}`, reviewedMergeRoute, loopMergeRepo, loopHead, reason), github.SquashRed
					}
					return "", 0
				}
			}
			r.pass(t)
			if r.called(gateCmdName) == nil || r.called(agentsName, agentWakeName, loopWorker) != nil {
				t.Fatalf("expected gate validation without repair: %v", r.calls)
			}
			if len(r.logged(t, "merge.failure-dropped")) != 1 || strings.Count(r.out.String(), "SUPERVISOR MERGE DROPPED") != 1 {
				t.Fatalf("missing single drop diagnostic: %s", r.out)
			}
		})
	}
}

func TestLoopRetiresStalePersistedRepair(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		for _, tested := range []string{loopHead, loopFixed} {
			t.Run(fmt.Sprintf("dry=%v/tested=%s", dryRun, tested), func(t *testing.T) {
				r := newLoopRig(t)
				l := &supervisorLoop{a: r.a, dryRun: dryRun}
				key := "go " + loopPR + "@" + loopHead + " reopened episode"
				stage := key + " repair"
				stages := map[string]bool{stage: true}
				details := map[string]string{stage: mergeFailureForTest(shortSHA(tested))}
				taken := []superloop.Taken{{Key: key}}
				got, err := l.dropInvalidMergeRepairs(taken, stages, details)
				if err != nil {
					t.Fatal(err)
				}
				stale := tested != loopHead
				if (len(got) == 0) != stale || stages[stage] == stale {
					t.Fatalf("wrong recovery: taken=%v stages=%v", got, stages)
				}
				want := 0
				if stale && !dryRun {
					want = 1
				}
				for _, verb := range []string{loopUnstageVerb, loopClaimEndVerb, "merge.failure-dropped"} {
					if n := len(r.logged(t, verb)); n != want {
						t.Fatalf("%s events = %d, want %d", verb, n, want)
					}
				}
				if _, err := l.dropInvalidMergeRepairs(got, stages, details); err != nil {
					t.Fatal(err)
				}
				if n := len(r.logged(t, "merge.failure-dropped")); n != want {
					t.Fatalf("repeated drop diagnostic: %d", n)
				}
			})
		}
	}
}
