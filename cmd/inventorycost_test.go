package cmd

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// Count the budget queries as well as inventory queries: even an unchanged
// REST response or a failed read has two fresh GraphQL budget samples.
func TestIdleInventoryGraphQLCallsBounded(t *testing.T) {
	for _, failure := range []string{"", "revision", "full inventory"} {
		t.Run("failure="+failure, func(t *testing.T) {
			r := newLoopRig(t)
			r.a.cfg.GitHub.Floor = 1000
			healthyLoopBudget(t, r)
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			start := r.a.now
			reset := start.Add(time.Hour)
			calls, revisions := 0, 0
			readLoopGraphQLBudget = func(*app, context.Context) (github.Budget, error) {
				calls++
				return github.Budget{Remaining: 4900, Reset: reset,
					GraphQL: &github.GraphQL{Limit: 5000, Remaining: 4900 - calls, Reset: reset}}, nil
			}
			ReadRepoPullRevision = func(_ context.Context, _, etag string) (string, bool, error) {
				revisions++
				if failure == "revision" {
					return "", false, errors.New("repository unavailable")
				}
				return "inventory-validator", etag != "", nil
			}
			readRepoOpenPulls = func(context.Context, string) ([]unmergedPull, error) {
				calls++
				if failure == "full inventory" {
					return nil, errors.New("GraphQL inventory unavailable")
				}
				return nil, nil
			}
			in := superloop.Input{Pulls: map[string]superloop.Pull{}}
			for n := range 3 {
				in.Pulls[fmt.Sprintf("o/repo%d#1", n)] = superloop.Pull{State: superloop.Open}
			}
			for pass := range 120 {
				r.a.now = start.Add(time.Duration(pass) * 30 * time.Second)
				before := calls
				if err := l.CheckUnmergedBacklogs(t.Context(), in); err != nil {
					t.Fatal(err)
				}
				// Three repositories, at most two probes and one query each.
				bound := 9
				if pass%10 != 0 {
					bound = 0
				}
				if calls-before > bound {
					t.Fatalf("idle pass %d: %d GraphQL calls, maximum %d", pass, calls-before, bound)
				}
			}
			if revisions != 36 || calls > 108 {
				t.Fatalf("hour: %d revisions, %d GraphQL calls; want 36 revisions and at most 108 calls", revisions, calls)
			}
		})
	}
}

func TestInventoryFailureNeverServesStalePullsAndRecovers(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	now := r.a.now
	l.Unmerged = map[string]LoopUnmergedObservation{repo: {
		Pulls: []unmergedPull{{Number: 1, CreatedAt: now}}, Etag: "inventory-cost-etag",
		At: now.Add(-UnmergedCacheTTL), FullAt: now,
	}}
	failure := errors.New("revision unavailable")
	reads := 0
	ReadRepoPullRevision = func(_ context.Context, _, etag string) (string, bool, error) {
		reads++
		if etag != "inventory-cost-etag" {
			t.Fatalf("lost last successful validator: %q", etag)
		}
		return etag, true, failure
	}
	readRepoOpenPulls = func(context.Context, string) ([]unmergedPull, error) {
		t.Fatal("unchanged recovery should reuse the successful payload")
		return nil, nil
	}
	for range 3 {
		pulls, _, err := l.RepoRecentUnmerged(t.Context(), repo)
		if !errors.Is(err, failure) || len(pulls) != 0 {
			t.Fatalf("failed read served stale data: %v, %v", pulls, err)
		}
	}
	if reads != 1 {
		t.Fatalf("failure retried %d times", reads)
	}
	// Restarting must not turn the failed observation into fresh usable data.
	l.SaveCache()
	l, err = r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.LoadCache()
	pulls, _, err := l.RepoRecentUnmerged(t.Context(), repo)
	if err == nil || err.Error() != failure.Error() || len(pulls) != 0 || reads != 1 {
		t.Fatalf("restart lost cached failure: %v, %v; %d reads", pulls, err, reads)
	}
	failure = nil
	r.a.now = now.Add(UnmergedCacheTTL)
	pulls, _, err = l.RepoRecentUnmerged(t.Context(), repo)
	if err != nil || len(pulls) != 1 || reads != 2 {
		t.Fatalf("recovery: %v, %v; %d reads", pulls, err, reads)
	}
}

func TestInventoryDoesNotCacheAdmissionOrCancellation(t *testing.T) {
	for _, failure := range []error{ErrLoopLater, context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			r := newLoopRig(t)
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ReadRepoPullRevision = func(context.Context, string, string) (string, bool, error) {
				if errors.Is(failure, context.Canceled) {
					cancel()
				}
				return "", false, failure
			}
			if _, _, err := l.RepoRecentUnmerged(ctx, repo); err == nil {
				t.Fatal("expected refusal")
			}
			if _, cached := l.Unmerged[repo]; cached {
				t.Fatal("transient admission or cancellation cached as repository failure")
			}
		})
	}
}
