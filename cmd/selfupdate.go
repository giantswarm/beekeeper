package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/update"
)

// runUpdate is the update; the tests replace it.
var runUpdate = update.Run

// selfUpdateCmdName is the self-update command's name.
const selfUpdateCmdName = "self-update"

// syncPluginsFlag runs only the plugin sync of the binary installed at the
// path, which a self-update hands over to.
const syncPluginsFlag = "sync-plugins"

// runInstalled runs the binary at path with args, its output to w; the
// tests replace it.
var runInstalled = func(ctx context.Context, w io.Writer, path string, args ...string) error {
	c := exec.CommandContext(ctx, path, args...) //nolint:gosec // the binary this update installed
	c.Stdout, c.Stderr = w, w
	return c.Run()
}

func (a *app) selfUpdateCmd() *cobra.Command {
	var check, pluginsOnly bool
	c := &cobra.Command{
		Use:   selfUpdateCmdName,
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

The new binary is renamed over the old one in one step, and every
beekeeper started afterwards is the new one. A running beekeeper watch (the
standby service's included) and a reopen waiting to show a session restart
on it once, each at its next quiet moment, as beekeeper install has them
do; the update names each (restart). It names the other beekeeper processes
it leaves on the old binary (pid and command), an alerts or guide watch
with the command that re-arms it: once the new release writes the state,
their saves are refused until they end or are restarted (beekeeper doctor
lists them meanwhile). An alerts or guide watch never re-executes itself:
its owner re-arms it, told by its own feed (WATCH STALE, GUIDE WATCH
STALE).

The beekeeper plugin goes with the binary: an update brings every
installed beekeeper plugin up to its marketplace's latest (claude plugin
marketplace update, then claude plugin update in the plugin's scope), so
the role skills it serves are the ones the new binary hands its workers;
on the latest release a plugin whose skills differ from the binary's is
brought up the same way. The new binary syncs its plugin itself: the
update runs it for that, and a binary installed by a release without the
sync (or by hand) syncs on the first command it runs. Only the beekeeper
on PATH syncs: a binary run from elsewhere (a downloaded release candidate)
leaves the plugin as it is. A plugin that stays
is said with the command that updates it. GitHub is asked
anonymously, apart from the budget gh and devctl share, unless GITHUB_TOKEN
is set. A development build (version dev) is refused.`,
		Args: cobra.NoArgs,
		// Needs neither the configuration nor the state.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			a.out = cmd.OutOrStdout()
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if pluginsOnly {
				return a.syncInstalledPlugins(cmd.Context())
			}
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
					stale := staleBinaries(t, os.Getpid(), replacedBinary)
					if a.loadConfig() == nil {
						if restarted, rerr := a.restartReplaced(w, res.Path, t, false); rerr == nil {
							stale = slices.DeleteFunc(stale, func(p *proc.Process) bool { return slices.Contains(restarted, p) })
						}
					}
					_, _ = fmt.Fprint(w, leftBehind(stale, res.Current, res.Latest))
				}
			}
			switch {
			case err != nil || check:
			case res.Updated:
				a.syncByInstalled(cmd.Context(), w, res.Path)
			default:
				syncPlugins(cmd.Context(), w, false)
			}
			return err
		},
	}
	c.Flags().BoolVar(&check, "check", false, "only report the running and the latest release; exit 125 when a newer one exists")
	c.Flags().BoolVar(&pluginsOnly, syncPluginsFlag, false, "only bring the plugin up to date for this binary, once")
	_ = c.Flags().MarkHidden(syncPluginsFlag)
	return c
}

// syncByInstalled has the binary an update installed at path bring the plugins up
// to date with its own sync, so they follow the new binary whatever this
// one knows; a new binary that does not run is said, and this one syncs.
func (a *app) syncByInstalled(ctx context.Context, w io.Writer, path string) {
	args := []string{selfUpdateCmdName, "--" + syncPluginsFlag}
	if a.cfgPath != "" {
		args = append(args, "--"+configFlag, a.cfgPath)
	}
	if err := runInstalled(ctx, w, path, args...); err != nil {
		_, _ = fmt.Fprintf(w, "The installed %s did not sync its plugin (%v): this binary syncs it.\n", path, err)
		syncPlugins(ctx, w, true)
	}
}

// syncInstalledPlugins is self-update --sync-plugins: this binary's plugin
// sync, once, recorded in the state directory.
func (a *app) syncInstalledPlugins(ctx context.Context) error {
	if err := a.loadConfig(); err != nil {
		return err
	}
	if err := os.MkdirAll(a.cfg.StateDir, 0o700); err != nil {
		return err
	}
	switch {
	case binaryIdentity() == "":
		_, _ = fmt.Fprintln(a.out, "A development build leaves the beekeeper plugin as it is.")
	case !runsInstalled():
		_, _ = fmt.Fprintln(a.out, "This beekeeper is not the one on PATH: it leaves the beekeeper plugin as it is.")
	case !syncOnce(ctx, a.out, a.cfg.StateDir):
		_, _ = fmt.Fprintf(a.out, "beekeeper %s synced no plugin: it did so before, another beekeeper does now, or Claude Code lists none.\n", binaryIdentity())
	}
	return nil
}

// leftBehind names the beekeeper processes an update leaves on the old
// binary, one line each, and what becomes of their saves; a gate call or a
// merge-child re-executes the installed binary at its next step, and says
// so, and a watch (beekeeper watch, alerts watch, guide watch) names the
// command that re-arms it. "" for none.
func leftBehind(stale []*proc.Process, old, installed string) string {
	if len(stale) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Left on %s until restarted, their saves of the state refused once %s writes it:\n", old, installed)
	for _, p := range stale {
		fmt.Fprintf(&b, "  pid %d: %s", p.PID, display(p.Args))
		switch {
		case reexecs(p):
			fmt.Fprintf(&b, " (re-executes %s at its next step)", installed)
		case isWatch(p):
			fmt.Fprintf(&b, " (a watch: %s)", rearm(p.Args))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// reexecs says whether the beekeeper process p re-executes the binary
// installed at its path by itself once replaced: a gate call, waiting for
// its turn or following its devctl, and a merge-child.
func reexecs(p *proc.Process) bool {
	return len(p.Args) > 1 && (p.Args[1] == gateCmdName || p.Args[1] == mergeChildCmd)
}
