package worktrees

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
)

func TestAutomaticRequiresEndedTaskAndPreservesWork(t *testing.T) {
	now := time.Now()
	base := Tree{Repo: "example/app", Branch: "feature", Touched: now.Add(-24 * time.Hour), Build: []string{"target/"}}
	merged := &github.HeadPull{State: github.Merged, ClosedAt: now.Add(-24 * time.Hour)}
	closed := &github.HeadPull{State: github.Closed, ClosedAt: now.Add(-24 * time.Hour)}
	for _, tc := range []struct {
		name                string
		pull                *github.HeadPull
		gone, harness, live bool
		work                []string
		unpushed            int
		want                string
	}{
		{name: "merged", pull: merged, want: Remove},
		{name: "deleted", gone: true, want: Remove},
		{name: "harness", harness: true, want: Remove},
		{name: "live harness", harness: true, live: true, want: Keep},
		{name: "closed branch remains", pull: closed, want: Keep},
		{name: "unknown task", want: Keep},
		{name: "dirty merged", pull: merged, work: []string{"source"}, want: Clean},
		{name: "unpublished deleted", gone: true, unpushed: 1, want: Clean},
		{name: "dirty harness", harness: true, work: []string{"source"}, want: Clean},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := base
			tree.Work, tree.Unpushed = tc.work, tc.unpushed
			got := Automatic(tree, tc.pull, tc.gone, tc.harness, tc.live, false, now)
			if got.Action != tc.want {
				t.Fatalf("%+v, want %s", got, tc.want)
			}
		})
	}
}

func TestRemoteGoneRequiresSuccessfulRemoteRead(t *testing.T) {
	for _, tc := range []struct {
		out  string
		err  error
		want bool
	}{
		{out: "", want: true}, {out: "object refs/heads/feature\n", want: false}, {err: errors.New("unavailable"), want: false},
	} {
		git := func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if len(args) != 4 || args[0] != "ls-remote" || args[3] != "refs/heads/feature" {
				t.Fatalf("unexpected remote query: %v", args)
			}
			return []byte(tc.out), tc.err
		}
		gone, err := RemoteGone(context.Background(), git, Tree{Branch: "feature"})
		if gone != tc.want || (err != nil) != (tc.err != nil) {
			t.Fatalf("gone=%v err=%v", gone, err)
		}
	}
}
