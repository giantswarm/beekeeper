package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/update"
)

// runUpdate is the update; the tests replace it.
var runUpdate = update.Run

func (a *app) selfUpdateCmd() *cobra.Command {
	var check bool
	c := &cobra.Command{
		Use:   "self-update",
		Short: "Replace this binary with the latest signed release (--check only asks)",
		Long: `Look up the latest release of ` + update.Repository + ` and, when it is newer
than this binary, install its binary for this OS and architecture over the
running executable. --check only reports both versions: exit 125 when a
newer release exists, 0 when this is the latest.

Release binaries are signed in CI (cosign, keyless) and published next to
their Sigstore bundle. The download is installed only after that bundle
verifies for a CircleCI build of ` + update.Repository + `; a release without
a bundle, or a download that does not match it, is refused and the binary
stays as it is.

The new binary is renamed over the old one in one step: a running
beekeeper watch keeps running (the old binary, until it is restarted), and
every beekeeper started afterwards is the new one. The update names the
beekeeper processes it leaves on the old binary (pid and command): once the
new release writes the state, their saves are refused until they end or are
restarted (beekeeper doctor lists them meanwhile). GitHub is asked
anonymously, apart from the budget gh and devctl share, unless GITHUB_TOKEN
is set. A development build (version dev) is refused.`,
		Args: cobra.NoArgs,
		// Needs neither the configuration nor the state.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			a.out = cmd.OutOrStdout()
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := a.out
			if a.json {
				w = io.Discard
			}
			res, err := runUpdate(cmd.Context(), w, check)
			if a.json && res.Latest != "" {
				if perr := a.printJSON(res); perr != nil {
					return perr
				}
			}
			if errors.Is(err, update.ErrOutdated) {
				return &exitError{code: ExitOutdated, msg: res.Latest + " is newer than " + res.Current + ": beekeeper self-update installs it"}
			}
			if err == nil && res.Updated {
				if t, terr := proc.Read(); terr == nil {
					_, _ = fmt.Fprint(w, leftBehind(staleBinaries(t, os.Getpid(), replacedBinary), res.Current, res.Latest))
				}
			}
			return err
		},
	}
	c.Flags().BoolVar(&check, "check", false, "only report the running and the latest release; exit 125 when a newer one exists")
	return c
}

// leftBehind names the beekeeper processes an update leaves on the old
// binary, one line each, and what becomes of their saves; a gate call or a
// merge-child re-executes the installed binary at its next step, and says
// so. "" for none.
func leftBehind(stale []*proc.Process, old, installed string) string {
	if len(stale) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Left on %s until restarted, their saves of the state refused once %s writes it:\n", old, installed)
	for _, p := range stale {
		fmt.Fprintf(&b, "  pid %d: %s", p.PID, display(p.Args))
		if reexecs(p) {
			fmt.Fprintf(&b, " (re-executes %s at its next step)", installed)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// reexecs says whether the beekeeper process p re-executes the binary
// installed at its path by itself once replaced: a gate call, waiting for
// its turn or following its devctl, and a merge-child.
func reexecs(p *proc.Process) bool {
	return len(p.Args) > 1 && (p.Args[1] == "gate" || p.Args[1] == mergeChildCmd)
}
