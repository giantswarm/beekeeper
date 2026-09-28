package cmd

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/github"
)

// squashMergeCmd is the hidden command the gate runs instead of devctl pr
// merge for a repository devctl does not serve (merge.devctlOwners).
const squashMergeCmd = "squash-merge"

// squashTimeout is the plain squash merge's wait for green, devctl pr
// merge's default.
const squashTimeout = 45 * time.Minute

// squashPoll is how often the plain squash merge reads the head's checks.
var squashPoll = 30 * time.Second

func (a *app) squashMergeCmd() *cobra.Command {
	var timeout time.Duration
	c := &cobra.Command{
		Use:    squashMergeCmd + " <owner/repo> <n>",
		Short:  "The gate's plain squash merge for a repository devctl does not serve",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := strconv.Atoi(args[1])
			if err != nil || n <= 0 {
				return exitCode(github.SquashTooling)
			}
			doc := github.Squash{GH: github.RunGH, Repo: args[0], Number: n, Timeout: timeout, Poll: squashPoll,
				Progress: os.Stderr, Now: time.Now, Sleep: time.Sleep}.Run(cmd.Context())
			raw, _ := json.MarshalIndent(doc, "", "  ")
			_, _ = os.Stdout.Write(append(raw, '\n'))
			return exitCode(doc.ExitCode)
		},
	}
	c.Flags().DurationVar(&timeout, "timeout", squashTimeout, "how long to wait for the head's checks")
	return c
}

// squashArgv is the gate's command for repo#pr on the plain squash merge
// route, with the caller's --timeout.
func squashArgv(self, repo string, pr int, devctlArgv []string) []string {
	argv := []string{self, squashMergeCmd, repo, strconv.Itoa(pr)}
	for i, a := range devctlArgv {
		if v, ok := strings.CutPrefix(a, "--timeout="); ok {
			return append(argv, "--timeout", v)
		}
		if a == "--timeout" && i+1 < len(devctlArgv) {
			return append(argv, "--timeout", devctlArgv[i+1])
		}
	}
	return argv
}
