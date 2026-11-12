package cmd

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/worktrees"
)

func publishedParkHead(ctx context.Context, on, dir string, git worktrees.Git, gh github.GH) string {
	head, _ := Cmd.PublishedParkVerification(ctx, on, dir, git, gh)
	return head
}
