package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// runUpdate is the update; the tests replace it.
var runUpdate = update.Run

// selfUpdateCmdName is the self-update command's name.
const selfUpdateCmdName = "self-update"

// verbSelfUpdateCanary is the event logged when a candidate release fails the
// self-update canary and is skipped.
const verbSelfUpdateCanary = "self-update.canary"

// skippedReleasesFile records releases whose self-update canary failed so they
// are skipped on later checks and updates.
const skippedReleasesFile = "skipped-releases.json"

// budgetGateArgs is the command that checks the GitHub budget and active gates.
var budgetGateArgs = strings.Fields("budget --gate")

// canaryChecks are the commands a candidate release must pass with the live
// configuration before it replaces the running binary.
var canaryChecks = [][]string{
	{"version"},
	budgetGateArgs,
	strings.Fields("supervisor status"),
	{"capacity"},
}

// runCanaryCheck runs one canary command on the staged binary at path and
// returns its combined output; the tests replace it.
var runCanaryCheck = func(ctx context.Context, path string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := proc.Command(cctx, path, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

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
	var check, pluginsOnly, completeProbation, rollback bool
	var rollbackReason, version string
	c := &cobra.Command{
		Use:   selfUpdateCmdName,
		Short: "Replace this binary with the latest signed release (--check only asks)",
		Long: `Look up the latest release of ` + update.Repository + ` and, when it is newer
than this binary, install its binary for this OS and architecture over the
running executable. A pre-release (a release candidate) looks among the
pre-releases too, a release only among releases. --check only reports both
versions: exit 125 when a newer release exists, 0 when this is the latest.

--version <tag> selects an exact release, including a release candidate
(for example --version v0.125.0-rc.3), even when older than this binary.
--check --version <tag> reports that release without installing it.

Release binaries are signed in CI (cosign, keyless) and published next to
their Sigstore bundle. The download is installed only after that bundle
verifies for a CircleCI build of ` + update.Repository + ` (certificate identity
https://circleci.com/api/v2/projects/..., issuer https://oidc.circleci.com, never a
GitHub identity); a release without
a bundle, or a download that does not match it, is refused and the binary
stays as it is.

The installation journal and verified recovery bytes are retained beside the
executable. After observing healthy operation, --complete-probation runs the
local health checks again and promotes the candidate to last-known-good.
Until then another update is refused. The first protected install has no
verified recovery target. --rollback --rollback-reason restores the previous
verified binary only when the live state writer is compatible; it never
restores a state snapshot. Recovery fences newer writers, including processes
still running the candidate. State already advanced by a newer release needs
an explicit lossless migration or a compatible signed release instead.
The executable directory must be writable; installation fails safely rather
than bypassing the journal with a privileged rename.

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

The agent sandbox's broker (beekeeper-sandbox.service) that runs the
replaced binary is restarted onto the new one (restart), after preparing
its vault session handover. An unavailable handover leaves it running. A
broker serving calls (a gate, a secret call, an agents start) is left to
finish them, and the restart still due is named with its command (due).

The beekeeper plugin goes with the binary: an update brings every
installed beekeeper plugin to this binary's release tag (claude plugin
marketplace update, then claude plugin update in the plugin's scope), so
the role skills it serves are the ones the new binary hands its workers;
on the latest release a plugin whose skills differ from the binary's is
brought up the same way. The new binary syncs its plugin itself: the
update runs it for that, and a binary installed by a release without the
sync (or by hand) syncs on the first command it runs. Only the beekeeper
on PATH syncs: a binary run from elsewhere (a downloaded release candidate)
leaves the plugin as it is. A plugin that stays
is said with the command that updates it. GitHub is asked as the person,
with GH_TOKEN, GITHUB_TOKEN or the gh login's token, so the lookup counts in
the budget gh and devctl share (beekeeper budget); without any token the
command says so and stops, never asking anonymously, whose limit the
machine's IP shares. A refused lookup names GitHub's rate limit with its
reset. A development build (version dev) is refused.`,
		Args: cobra.NoArgs,
		// Needs neither the configuration nor the state.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			a.out = cmd.OutOrStdout()
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if completeProbation || rollback {
				path, err := os.Executable()
				if err != nil {
					return err
				}
				if completeProbation {
					if err := a.canaryRelease(cmd.Context(), a.out, path, project.Version()); err != nil {
						if qerr := update.Quarantine(cmd.Context(), path, "post-install health checks failed"); qerr != nil {
							return errors.Join(err, qerr)
						}
						return err
					}
					if err := a.loadConfig(); err != nil {
						return err
					}
					store, err := state.Open(a.cfg.StateDir)
					if err != nil {
						return err
					}
					if err := update.CompleteProbation(cmd.Context(), path, project.Version()); err != nil {
						return err
					}
					return store.ResumeRecoveredWrites(project.Version())
				}
				if err := a.loadConfig(); err != nil {
					return err
				}
				store, err := state.Open(a.cfg.StateDir)
				if err != nil {
					return err
				}
				if err := update.Rollback(cmd.Context(), path, rollbackReason, store.RecoverVersion); err != nil {
					return err
				}
				_, err = fmt.Fprintln(a.out, "Restored the verified recovery binary. Restart candidate processes at a quiet moment; manually re-arm alerts and guide watches. Newer state writers remain fenced.")
				return err
			}
			if pluginsOnly {
				return a.syncInstalledPlugins(cmd.Context())
			}
			w := a.out
			if a.json {
				w = io.Discard
			}
			if !check {
				a.warnUpdateConfig(w)
			}
			started := time.Now()
			ctx := update.WithOptions(cmd.Context(), update.Options{
				Version: version,
				Canary: func(ctx context.Context, path, version string) error {
					return a.canaryRelease(ctx, w, path, version)
				},
				Skip: a.isSkippedRelease,
			})
			res, err := runUpdate(ctx, w, check)
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
					configured := a.loadConfig() == nil
					if configured {
						if restarted, rerr := a.restartReplaced(w, res.Path, t, false); rerr == nil {
							stale = slices.DeleteFunc(stale, func(s staleBinary) bool { return slices.Contains(restarted, s.Process) })
						}
					}
					left := make([]*proc.Process, len(stale))
					for i, s := range stale {
						left[i] = s.Process
					}
					_, _ = fmt.Fprint(w, leftBehind(left, res.Current, res.Latest))
					if configured {
						restartBrokerOn(cmd.Context(), w, res.Path, started, t, false, a.cfg.Secret.Session)
					} else {
						_, _ = fmt.Fprintln(w, "due     broker restart refused: configuration could not be read safely")
					}
				}
			}
			switch {
			case !syncsUnder(a.explicitConfig()):
			case err != nil || check:
			case res.Updated:
				a.syncByInstalled(cmd.Context(), w, res.Path)
			default:
				syncPlugins(cmd.Context(), w, false)
			}
			return err
		},
	}
	c.Flags().StringVar(&version, "version", "", "install an exact release tag, including a pre-release (works with --check)")
	c.Flags().BoolVar(&completeProbation, "complete-probation", false, "run health checks and promote the installed candidate to last-known-good")
	c.Flags().BoolVar(&rollback, "rollback", false, "restore the verified previous binary only if live state is compatible")
	c.Flags().StringVar(&rollbackReason, "rollback-reason", "", "durable quarantine reason for the failed candidate")
	c.Flags().BoolVar(&check, "check", false, "only report the running and the latest release; exit 125 when a newer one exists")
	c.Flags().BoolVar(&pluginsOnly, syncPluginsFlag, false, "only bring the plugin up to date for this binary, once")
	c.MarkFlagsMutuallyExclusive("check", "complete-probation", "rollback", syncPluginsFlag)
	c.MarkFlagsMutuallyExclusive("version", "complete-probation", "rollback", syncPluginsFlag)
	c.MarkFlagsRequiredTogether("rollback", "rollback-reason")
	_ = c.Flags().MarkHidden(syncPluginsFlag)
	return c
}

// syncByInstalled has the binary an update installed at path bring the plugins up
// to date with its own sync, so they follow the new binary whatever this
// one knows; a new binary that does not run leaves the sync to its next run.
func (a *app) syncByInstalled(ctx context.Context, w io.Writer, path string) {
	args := []string{selfUpdateCmdName, "--" + syncPluginsFlag}
	if a.cfgPath != "" {
		args = append(args, "--"+configFlag, a.cfgPath)
	}
	if err := runInstalled(ctx, w, path, args...); err != nil {
		_, _ = fmt.Fprintf(w, "The installed %s did not sync its plugin (%v): its next command will retry.\n", path, err)
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
	case !syncsUnder(a.explicitConfig()):
		_, _ = fmt.Fprintln(a.out, "A scratch configuration leaves the beekeeper plugin as it is.")
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
	sub := claude.BeekeeperSubcommand(p)
	return sub == gateCmdName || sub == mergeChildCmd
}

func (a *app) selfUpdateStore() state.Store {
	if a.store != nil {
		return a.store
	}
	if a.cfg == nil || a.cfg.StateDir == "" {
		_ = a.loadConfig()
	}
	var dir string
	if a.cfg != nil && a.cfg.StateDir != "" {
		dir = a.cfg.StateDir
	} else if s := os.Getenv("XDG_STATE_HOME"); s != "" {
		dir = filepath.Join(s, "beekeeper")
	} else if h, err := os.UserHomeDir(); err == nil {
		dir = filepath.Join(h, ".local", "state", "beekeeper")
	}
	if dir == "" {
		return nil
	}
	s, err := state.Open(dir)
	if err != nil {
		return nil
	}
	a.store = s
	return s
}

func (a *app) isSkippedRelease(tag string) bool {
	s := a.selfUpdateStore()
	if s == nil {
		return false
	}
	var skipped map[string]string
	found, err := s.ReadFile(skippedReleasesFile, &skipped)
	if err != nil || !found {
		return false
	}
	tag = "v" + strings.TrimPrefix(tag, "v")
	return skipped[tag] != ""
}

func (a *app) skipRelease(version, why string) {
	s := a.selfUpdateStore()
	if s == nil {
		return
	}
	version = "v" + strings.TrimPrefix(version, "v")
	var skipped map[string]string
	_, _ = s.ReadFile(skippedReleasesFile, &skipped)
	if skipped == nil {
		skipped = map[string]string{}
	}
	skipped[version] = why
	_ = s.WriteFile(skippedReleasesFile, skipped)
	_ = s.Record(event(state.Party{Name: selfUpdateCmdName}, verbSelfUpdateCanary, "skipping %s: %s", version, why))
}

func (a *app) canaryRelease(ctx context.Context, w io.Writer, path, version string) error {
	for _, check := range canaryChecks {
		name := strings.Join(check, " ")
		args := check
		if a.cfgPath != "" {
			args = append([]string{"--" + configFlag, a.cfgPath}, check...)
		}
		out, err := runCanaryCheck(ctx, path, args...)
		if err != nil {
			detail := oneLine(out)
			if detail == "" {
				detail = oneLine(err.Error())
			}
			why := fmt.Sprintf("canary %s failed: %s", name, detail)
			a.skipRelease(version, why)
			_, _ = fmt.Fprintf(w, "Skipping %s (%s); keeping the current binary.\n", version, why)
			return fmt.Errorf("skipping %s: %s", version, why)
		}
	}
	return nil
}

// warnUpdateConfig runs before handing control to a newer binary, which may
// understand configuration that this installed version cannot read.
func (a *app) warnUpdateConfig(w io.Writer) {
	_, _ = fmt.Fprintln(w, "WARNING: configuration changes made with a newer release may be unreadable by the currently installed binary. Before changing configuration, finish the update and restart older beekeeper processes; validate configuration before reverting.")

	if err := a.loadConfig(); err != nil {
		_, _ = fmt.Fprintf(w, "WARNING: the installed binary cannot read the configuration: %v. A newer release may accept or write values this version cannot read; repair the configuration before reverting.\n", err)
	}
}
