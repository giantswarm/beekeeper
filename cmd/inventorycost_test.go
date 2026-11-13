package cmd

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// Count the budget readings as well as the inventory queries. The
// conditional REST read of the revision takes no GraphQL reading, unchanged
// or failed: only the GraphQL read of the open pull requests is counted
// between two, on a repository's first read, after a change or after its
// own failure (#3769).
func TestIdleInventoryGraphQLCallsBounded(t *testing.T) {
	for _, tc := range []struct {
		failure string
		// first is the GraphQL calls of the first pass, later those of each
		// pass that reads the three repositories again.
		first, later int
	}{
		{failure: "", first: 9},
		{failure: "revision"},
		{failure: "full inventory", first: 9, later: 9},
	} {
		t.Run("failure="+tc.failure, func(t *testing.T) {
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
			loop.ReadRepoPullRevision = func(_ context.Context, _, etag string) (string, bool, error) {
				revisions++
				if tc.failure == "revision" {
					return "", false, errors.New("repository unavailable")
				}
				return "inventory-validator", etag != "", nil
			}
			readRepoOpenPulls = func(context.Context, string) ([]unmergedPull, error) {
				calls++
				if tc.failure == "full inventory" {
					return nil, errors.New("GraphQL inventory unavailable")
				}
				return nil, nil
			}
			in := superloop.Input{Pulls: map[string]superloop.Pull{}}
			for n := range 3 {
				in.Pulls[fmt.Sprintf("o/repo%d#1", n)] = superloop.Pull{State: superloop.Open}
			}
			want := 0
			for pass := range 120 {
				r.a.now = start.Add(time.Duration(pass) * 30 * time.Second)
				before := calls
				if err := l.CheckUnmergedBacklogs(t.Context(), in); err != nil {
					t.Fatal(err)
				}
				// Three repositories, read again every ten passes: two
				// readings and one query each where a full read is due.
				expected := 0
				switch {
				case pass == 0:
					expected = tc.first
				case pass%10 == 0:
					expected = tc.later
				}
				want += expected
				if calls-before != expected {
					t.Fatalf("idle pass %d: %d GraphQL calls, want %d", pass, calls-before, expected)
				}
			}
			if revisions != 36 || calls != want {
				t.Fatalf("hour: %d revisions, %d GraphQL calls; want 36 revisions and %d calls", revisions, calls, want)
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
	l.Unmerged = map[string]loop.LoopUnmergedObservation{repo: {
		Pulls: []unmergedPull{{Number: 1, CreatedAt: now}}, Etag: "inventory-cost-etag",
		At: now.Add(-loop.UnmergedCacheTTL), FullAt: now,
	}}
	failure := errors.New("revision unavailable")
	reads := 0
	loop.ReadRepoPullRevision = func(_ context.Context, _, etag string) (string, bool, error) {
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
	r.a.now = now.Add(loop.UnmergedCacheTTL)
	pulls, _, err = l.RepoRecentUnmerged(t.Context(), repo)
	if err != nil || len(pulls) != 1 || reads != 2 {
		t.Fatalf("recovery: %v, %v; %d reads", pulls, err, reads)
	}
}

func TestInventoryDoesNotCacheAdmissionOrCancellation(t *testing.T) {
	for _, failure := range []error{loop.ErrLoopLater, context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			r := newLoopRig(t)
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			loop.ReadRepoPullRevision = func(context.Context, string, string) (string, bool, error) {
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
