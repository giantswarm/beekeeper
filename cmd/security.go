package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/github"
)

func (a *app) securityCmd() *cobra.Command {
	c := &cobra.Command{
		Use: "security", Short: "Read repository security findings with the worker's GitHub identity",
	}
	c.AddCommand(a.securityAlertCmd(github.RunGH))
	return c
}

func (a *app) securityAlertCmd(run github.GH) *cobra.Command {
	return &cobra.Command{
		Use: "alert <owner/repo> <number>", Short: "Read a Dependabot alert's state through the GitHub REST API",
		Long: `Read one Dependabot alert through gh's current identity, using REST.
In a worker sandbox this is the devctl App through the egress proxy.
The App needs repository permission Dependabot alerts: read
(vulnerability_alerts), accepted on its installation. No personal token
scope is added and no alternate identity is tried. Permission errors and
unavailable alerts fail rather than count as fixed. --json prints number,
state and html_url. See docs/security-scans.md for setup and the scan rule.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			number, err := strconv.Atoi(args[1])
			if err != nil || number <= 0 {
				return usageErr("expected owner/repo and a positive alert number")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			alert, err := github.ReadDependabotAlert(ctx, run, args[0], number)
			if err != nil {
				return err
			}
			if a.json {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(alert)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", alert.State, alert.URL)
			return err
		},
	}
}
