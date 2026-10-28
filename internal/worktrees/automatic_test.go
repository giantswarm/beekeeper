package worktrees

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
)

const (
	automaticFeature      = "feature"
	advertisedDefaultHead = "base refs/heads/main\n"
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
		{name: "cut from remote default", tracked: "main", out: advertisedDefaultHead},
		{name: "deleted unmerged branch", tracked: automaticFeature, out: advertisedDefaultHead, wantGone: true},
		{name: "closed unmerged pull", hadPull: true, out: advertisedDefaultHead, wantGone: true},
		{name: "published current head", tracked: automaticFeature, out: "work refs/heads/feature\n", wantPublished: true},
		{name: "unavailable", err: errors.New("unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git := func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if args[0] == "merge-base" {
					return nil, errors.New("not an ancestor")
				}
				if len(args) != 3 || args[0] != "ls-remote" || args[1] != "--heads" {
					t.Fatalf("unexpected query: %v", args)
				}
				return []byte(tc.out), tc.err
			}
			gone, published, err := RemoteStatus(context.Background(), git, Tree{Branch: automaticFeature, Head: "work", Pushed: tc.tracked}, tc.hadPull)
			if gone != tc.wantGone || published != tc.wantPublished || (err != nil) != (tc.err != nil) {
				t.Fatalf("gone=%v published=%v err=%v", gone, published, err)
			}
		})
	}
}
