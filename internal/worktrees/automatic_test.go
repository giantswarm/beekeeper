package worktrees

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
)

const (
	automaticFeature      = "feature"
	automaticDefault      = "main"
	advertisedDefaultHead = "base refs/heads/main\n"
	gitLsRemote           = "ls-remote"
	gitRevList            = "rev-list"
	remoteStatusHead      = "work"
)

func TestAutomaticRequiresEndedTaskAndPreservesWork(t *testing.T) {
	now := time.Now()
	base := Tree{Repo: "example/app", Branch: automaticFeature, Pushed: automaticFeature, Touched: now.Add(-24 * time.Hour), Build: []string{"target/"}}
	merged := &github.HeadPull{State: github.Merged, ClosedAt: now.Add(-24 * time.Hour)}
	closed := &github.HeadPull{State: github.Closed, ClosedAt: now.Add(-24 * time.Hour)}
	for _, tc := range []struct {
		name                string
		pull                *github.HeadPull
		gone, harness, live bool
		work                []string
		unpushed            int
		unpublished         bool
		untracked           bool
		want                string
	}{
		{name: "merged", pull: merged, want: Remove},
		{name: "deleted", gone: true, want: Remove},
		{name: "never tracked", gone: true, untracked: true, want: Keep},
		{name: "harness", harness: true, want: Remove},
		{name: "open harness", pull: &github.HeadPull{State: github.Open}, harness: true, gone: true, want: Keep},
		{name: "recently closed harness", pull: &github.HeadPull{State: github.Closed, ClosedAt: now.Add(-time.Hour)}, harness: true, want: Keep},
		{name: "stale tracking ref", pull: closed, gone: true, unpublished: true, want: Clean},
		{name: "unpublished harness", harness: true, unpublished: true, want: Clean},
		{name: "live harness", harness: true, live: true, want: Keep},
		{name: "closed branch remains", pull: closed, want: Keep},
		{name: "unknown task", want: Keep},
		{name: "dirty merged", pull: merged, work: []string{"source"}, want: Clean},
		{name: "unpublished deleted", gone: true, unpushed: 1, unpublished: true, want: Clean},
		{name: "dirty harness", harness: true, work: []string{"source"}, want: Clean},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := base
			if tc.untracked {
				tree.Pushed = ""
			}
			tree.Work, tree.Unpushed = tc.work, tc.unpushed
			got := Automatic(tree, tc.pull, tc.gone, tc.harness, tc.live, !tc.unpublished, now)
			if got.Action != tc.want {
				t.Fatalf("%+v, want %s", got, tc.want)
			}
		})
	}
}

func TestRemoteStatusUsesCurrentPublicationAndRequiresBranchHistory(t *testing.T) {
	for _, tc := range []struct {
		name, out, tracked               string
		hadPull, wantGone, wantPublished bool
		err                              error
	}{
		{name: "never pushed", out: advertisedDefaultHead},
		{name: "cut from remote default", tracked: automaticDefault, out: advertisedDefaultHead},
		{name: "deleted unmerged branch", tracked: automaticFeature, out: advertisedDefaultHead, wantGone: true},
		{name: "closed unmerged pull", hadPull: true, out: advertisedDefaultHead, wantGone: true},
		{name: "published current head", tracked: automaticFeature, out: "work refs/heads/feature\n", wantPublished: true},
		{name: "unavailable", err: errors.New("unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git := func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if args[0] == gitRevList {
					return []byte(remoteStatusHead + "\n"), nil
				}
				if len(args) != 3 || args[0] != gitLsRemote || args[1] != "--heads" {
					t.Fatalf("unexpected query: %v", args)
				}
				return []byte(tc.out), tc.err
			}
			gone, published, err := RemoteStatus(context.Background(), git, Tree{Branch: automaticFeature, Head: remoteStatusHead, Pushed: tc.tracked}, tc.hadPull)
			if gone != tc.wantGone || published != tc.wantPublished || (err != nil) != (tc.err != nil) {
				t.Fatalf("gone=%v published=%v err=%v", gone, published, err)
			}
		})
	}
}

// Hundreds of remote branches must not become hundreds of ancestry processes
// for each retained worktree. Both published and unpublished heads are bounded.
func TestRemoteStatusForkBudget(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(fmt.Sprint(published), func(t *testing.T) {
			var refs strings.Builder
			for i := range 323 {
				fmt.Fprintf(&refs, "%040x refs/heads/branch-%d\n", i+1, i)
			}
			calls := 0
			git := func(_ context.Context, _ string, args ...string) ([]byte, error) {
				calls++
				switch args[0] {
				case gitLsRemote:
					return []byte(refs.String()), nil
				case gitRevList:
					if published {
						return nil, nil
					}
					return []byte("local-head\n"), nil
				default:
					t.Fatalf("unexpected git: %v", args)
					return nil, nil
				}
			}
			gone, got, err := RemoteStatus(t.Context(), git, Tree{Head: "local-head", Branch: remoteStatusHead, Pushed: remoteStatusHead}, true)
			if err != nil || !gone || got != published {
				t.Fatalf("gone=%t published=%t err=%v", gone, got, err)
			}
			if calls > 2 {
				t.Fatalf("one worktree forked %d git processes; budget is 2", calls)
			}
		})
	}
}

func TestPressureExpiresOnlyEndedTaskGrace(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name                           string
		pull                           *github.HeadPull
		live, locked, dirty, published bool
		want                           string
	}{
		{name: "recent merged", pull: &github.HeadPull{State: github.Merged, ClosedAt: now}, published: true, want: Remove},
		{name: "dirty merged", pull: &github.HeadPull{State: github.Merged, ClosedAt: now}, dirty: true, published: true, want: Clean},
		{name: "unpublished", pull: &github.HeadPull{State: github.Merged, ClosedAt: now}, want: Clean},
		{name: "live", pull: &github.HeadPull{State: github.Merged, ClosedAt: now}, live: true, published: true, want: Keep},
		{name: "locked", pull: &github.HeadPull{State: github.Merged, ClosedAt: now}, locked: true, published: true, want: Keep},
		{name: "open", pull: &github.HeadPull{State: github.Open}, published: true, want: Keep},
		{name: "unknown", published: true, want: Keep},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := Tree{Touched: now, Locked: tc.locked, Build: []string{"target/"}}
			if tc.dirty {
				tree.Work = []string{"source.go"}
			}
			if got := AutomaticPressure(tree, tc.pull, false, false, tc.live, tc.published, now, true); got.Action != tc.want {
				t.Fatalf("got %+v, want %s", got, tc.want)
			}
		})
	}
}
