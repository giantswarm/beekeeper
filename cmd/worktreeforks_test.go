package cmd

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
)

// Repeated cleanup scans must reuse both positive and negative graph results.
// A new local commit invalidates the result, while dirty work is always retained.
func TestWorktreePublicationForkBudget(t *testing.T) {
	const source = "source.go"
	a, read, root := worktreeApp(t)
	m, _ := read()
	dir := filepath.Join(root, "nopr")
	writeT(t, filepath.Join(dir, source), "package feature\n")
	gitT(t, dir, "add", source)
	gitT(t, dir, "commit", "-qm", "feature")
	m.dirs, m.automatic = []string{dir}, true
	m.retire = func(context.Context, []string, bool) error { return nil }
	m.pulls = func(context.Context, []github.Head) (map[github.Head]github.HeadPull, error) {
		return map[github.Head]github.HeadPull{{Repo: "acme/app", Branch: "nopr"}: {State: github.Closed, ClosedAt: m.now.Add(-24 * time.Hour)}}, nil
	}
	graphs := 0
	git := m.git
	m.git = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		if args[0] == "ls-remote" {
			return git(ctx, dir, "ls-remote", "--heads", "local")
		}
		if args[0] == "merge-base" || len(args) > 1 && args[0] == "rev-list" && args[1] == "--max-count=1" {
			graphs++
		}
		return git(ctx, dir, args...)
	}
	scan := func() {
		t.Helper()
		r := a.trimWorktrees(true, func() (worktreeMachine, error) { return m, nil })
		if r.err != nil || len(r.removed) != 0 {
			t.Fatalf("unpublished work was not retained: %+v", r)
		}
	}
	for range 60 {
		scan()
	}
	if graphs != 1 {
		t.Fatalf("unchanged worktree needed %d graph processes, want 1", graphs)
	}
	writeT(t, filepath.Join(dir, source), "package changed\n")
	gitT(t, dir, "commit", "-qam", "another change")
	scan()
	if graphs != 2 {
		t.Fatalf("changed HEAD did not invalidate graph result: %d", graphs)
	}
	// The HEAD stays put, but the publication tip changes to a locally known
	// descendant. A dirty file must still prevent removal after publication.
	tree := gitT(t, dir, "rev-parse", "HEAD^{tree}")
	tip := gitT(t, dir, "commit-tree", tree, "-p", "HEAD", "-m", "published descendant")
	gitT(t, dir, "push", "-q", "local", tip+":main")
	writeT(t, filepath.Join(dir, "untracked.txt"), "keep this work\n")
	scan()
	if graphs != 3 {
		t.Fatalf("changed remote tip did not invalidate graph result: %d", graphs)
	}
	for range 10 {
		scan()
	}
	if graphs != 3 {
		t.Fatalf("published HEAD was queried repeatedly: %d", graphs)
	}
	if !exists(filepath.Join(dir, "untracked.txt")) {
		t.Fatal("dirty work lost")
	}
}
