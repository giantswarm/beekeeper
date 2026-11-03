package cmd

import (
	"context"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/state"
)

// gateCheckCmd is the gate's dry path for a pull request merge: the reviews
// read the gate makes before devctl runs, nothing queued, merged or logged.
func (a *app) gateCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check <owner/repo> <n>",
		Short: "Judge a pull request's reviews as the gate does before a merge, merging nothing",
		Long: `check reads the pull request's reviews and comments as the gate does before
devctl pr merge runs and refuses (77) while a reviewer holds the merge: a
request for changes that is the reviewer's latest verdict, or unanswered
feedback newer than the head. 74 is a read that failed in transit, 0 a merge
the reviews let through. Nothing queues, merges or reaches the state.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := strconv.Atoi(args[1])
			if err != nil || n <= 0 {
				return fmt.Errorf("%q is no pull request number", args[1])
			}
			return a.gateCheck(cmd.Context(), args[0], n)
		},
	}
}

// gateCheck runs the gate's reviews read for repo#n, its refusal the gate's
// line and exit code.
func (a *app) gateCheck(ctx context.Context, repo string, n int) error {
	g := &gateRun{app: a, ctx: ctx, repo: repo, pr: n}
	if err := g.checkReviews(gateRefusedWith); err != nil {
		return err
	}
	gateLine("dry path: %s would merge as far as its reviews go", state.Merge{Repo: repo, PR: n}.Key())
	return nil
}
