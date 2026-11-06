package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/github"
)

func (a *app) doctorRepositoryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "repository <owner>/<repo> [<owner>/<repo> ...]",
		Short: "Resolve repositories through the read-only per-owner GraphQL path",
		Long: `Resolve each repository through the same per-owner GraphQL authentication
routing used by the watch's note and park checks. Prints the resolved name
and owner type (User or Organization). Needs no note, park or registered caller.

Example: beekeeper doctor repository giantswarm/beekeeper`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, repos []string) error {
			resolved, err := github.Repositories(cmd.Context(), github.RunGH, repos)
			if err != nil {
				return err
			}
			if a.json {
				return json.NewEncoder(a.out).Encode(resolved)
			}
			for _, r := range resolved {
				if _, err := fmt.Fprintf(a.out, "%s (%s)\n", r.NameWithOwner, r.Owner.Type); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
