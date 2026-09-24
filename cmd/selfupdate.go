package cmd

import (
	"errors"
	"io"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/update"
)

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
every beekeeper started afterwards is the new one. GitHub is asked
anonymously, apart from the budget gh and devctl share, unless GITHUB_TOKEN
is set. A development build (version dev) is refused.`,
		Args: cobra.NoArgs,
		// Needs neither the configuration nor the state.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := a.out
			if a.json {
				w = io.Discard
			}
			res, err := update.Run(cmd.Context(), w, check)
			if a.json && res.Latest != "" {
				if perr := a.printJSON(res); perr != nil {
					return perr
				}
			}
			if errors.Is(err, update.ErrOutdated) {
				return &exitError{code: ExitOutdated, msg: res.Latest + " is newer than " + res.Current + ": beekeeper self-update installs it"}
			}
			return err
		},
	}
	c.Flags().BoolVar(&check, "check", false, "only report the running and the latest release; exit 125 when a newer one exists")
	return c
}
