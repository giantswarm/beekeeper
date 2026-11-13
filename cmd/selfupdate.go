package cmd

import (
	"cmp"
	"context"
	"encoding/json"
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
	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/install"
	"github.com/giantswarm/beekeeper/internal/loopworkflow"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// runUpdate is the update; the tests replace it.
var runUpdate = update.Run

// runStageCanary runs the staged release-candidate canary; the tests replace it.
var runStageCanary = loopworkflow.StageCanary

// selfUpdateCmdName is the self-update command's name.
const selfUpdateCmdName = "self-update"

const recoverOutdatedJudgeFlag = "recover-outdated-judge"

// verbSelfUpdateCanary is the event logged when a candidate release fails the
// self-update canary and is skipped.
const verbSelfUpdateCanary = "self-update.canary"

// verbSelfUpdateComparison is the event logged when a candidate release goes
// past the dry-run comparison uncompared: the running release's own pass gave
// no baseline.
const verbSelfUpdateComparison = "self-update.comparison"

// skippedReleasesFile records releases whose self-update canary failed so they
// are skipped on later checks and updates.
const skippedReleasesFile = "skipped-releases.json"

// budgetGateArgs is the command that checks the GitHub budget and active gates.
var budgetGateArgs = strings.Fields("budget --gate")

// canaryChecks are the commands a candidate release must pass with the live
// configuration before it replaces the running binary.
var canaryChecks = [][]string{
	{hostVersionFlag},
	budgetGateArgs,
	strings.Fields("supervisor status"),
	{"capacity"},
}

// runCanaryCheck runs one canary command on the staged binary at path and
// returns its combined output; the tests replace it.
var runCanaryCheck = func(ctx context.Context, path string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
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
	var check, pluginsOnly, completeProbation, rollback, controller, clearHold, canaryRecorded bool
	var rollbackReason, version, recoveryTarget, recoverJudge string
	var stageCanary, canaryBundle, canaryRepo, canaryAllowOrg, canaryRun, canaryScenario, canaryOutput string
	var allowLoopRegression bool
	var canaryProject int
	var canarySiblings []string
	var every, step time.Duration
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
executable, or in this account's state when it cannot write there. The next
update run by the installed candidate repeats the local health checks and
promotes it to last-known-good; a failed check quarantines it and the update
moves past it. --complete-probation does the same at once, and alone lifts a
recovery fence. The first protected install has no verified recovery target.
--rollback --rollback-reason restores the previous
verified binary only when the live state writer is compatible; it never
restores a state snapshot. Recovery fences newer writers, including processes
still running the candidate. State already advanced by a newer release needs
an explicit lossless migration or a compatible signed release instead.
An update launches a recovery controller from the retained last-known-good
bytes before it replaces the binary. It evaluates the candidate every minute:
a failure streak, a failed isolated workflow probe or a missing loop heartbeat
restores the recovery target and quarantines the candidate within 30 minutes,
and a healthy probation promotes it. An update waits for that verdict. A
recovery that cannot complete holds updates and worker starts and reports why;
--clear-recovery-hold lifts the hold once that is dealt with.
An executable directory this account cannot write takes sudo -n for the final
replacement alone: the journal is written first, and nothing else runs with
privilege.

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
			if stageCanary != "" {
				return a.runStagedCanary(cmd.Context(), loopworkflow.CanaryConfig{
					Candidate: stageCanary,
					Bundle:    canaryBundle,
					Recorded:  canaryRecorded,
					Repo:      canaryRepo,
					AllowOrg:  canaryAllowOrg,
					Project:   canaryProject,
					Run:       canaryRun,
					Scenario:  canaryScenario,
					Siblings:  canarySiblings,
				}, canaryOutput)
			}
			if recoverJudge != "" {
				return a.recoverOutdatedJudge(cmd.Context(), recoverJudge)
			}
			if controller {
				return a.runRecoveryController(cmd.Context(), a.out, recoveryTarget, every, cmp.Or(step, every))
			}
			if clearHold {
				path, err := os.Executable()
				if err != nil {
					return err
				}
				return a.clearRecoveryHold(cmd.Context(), path)
			}
			if completeProbation || rollback {
				path, err := os.Executable()
				if err != nil {
					return err
				}
				if completeProbation {
					if err := probationHealth(cmd.Context(), a.cfgPath, path); err != nil {
						if errors.Is(err, update.ErrHealthUnproven) {
							return err
						}
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
			started := clk.Now()
			ctx := update.WithOptions(cmd.Context(), update.Options{
				Version: version,
				Canary: func(ctx context.Context, path, version string) error {
					if err := a.canaryRelease(ctx, w, path, version); err != nil {
						return err
					}
					if allowLoopRegression {
						return nil
					}
					current, err := os.Executable()
					if err != nil {
						return fmt.Errorf("locating running release for dry-run comparison: %w", err)
					}
					note, err := compareLoopPasses(ctx, current, path, a.cfgPath)
					if err == nil && note != "" {
						a.noteUncompared(w, version, note)
					}
					return err
				},
				Health: func(ctx context.Context, path, _ string) error {
					return probationHealth(ctx, a.cfgPath, path)
				},
				Skip:     a.isSkippedRelease,
				Recovery: a.launchRecovery,
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
						} else {
							_, _ = fmt.Fprintf(w, "due     process restart request failed: %v\n", rerr)
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
				if err := a.loadConfig(); err != nil {
					return err
				}
				if err := install.RefreshSkills(a.cfg.StateDir); err != nil {
					return err
				}
				syncPlugins(cmd.Context(), w, false)
			}
			return err
		},
	}
	c.Flags().StringVar(&version, hostVersionFlag, "", "install an exact release tag, including a pre-release (works with --check)")
	c.Flags().StringVar(&recoverJudge, recoverOutdatedJudgeFlag, "", "reinstall and promote this verified journal candidate after an outdated judge rollback (installed executable path)")
	c.Flags().BoolVar(&completeProbation, "complete-probation", false, "run health checks and promote the installed candidate to last-known-good")
	c.Flags().BoolVar(&rollback, "rollback", false, "restore the verified previous binary only if live state is compatible")
	c.Flags().StringVar(&rollbackReason, "rollback-reason", "", "durable quarantine reason for the failed candidate")
	c.Flags().BoolVar(&check, "check", false, "only report the running and the latest release; exit 125 when a newer one exists")
	c.Flags().BoolVar(&pluginsOnly, syncPluginsFlag, false, "only bring the plugin up to date for this binary, once")
	c.Flags().BoolVar(&clearHold, "clear-recovery-hold", false, "lift the hold of a failed recovery once its cause is dealt with")
	c.Flags().BoolVar(&allowLoopRegression, "allow-loop-regression", false, "install despite an intentional dry-run transition regression (signature and canary checks still apply)")
	c.Flags().StringVar(&stageCanary, "stage-canary", "", "run a staged release candidate through the isolated sandbox canary and emit canary evidence")
	c.Flags().StringVar(&canaryBundle, "canary-bundle", "", "cosign bundle to verify against the staged candidate before running the canary")
	c.Flags().BoolVar(&canaryRecorded, "canary-recorded", false, "run the staged canary against the isolated recorded fixture origin")
	c.Flags().StringVar(&canaryRepo, "canary-repo", "", "explicit allowlisted sandbox repository (<owner>/<repo>) for the staged canary")
	c.Flags().StringVar(&canaryAllowOrg, "canary-allow-org", "", "explicit allowlisted organization owning --canary-repo")
	c.Flags().IntVar(&canaryProject, "canary-project", 0, "explicit sandbox GitHub ProjectV2 number for the staged canary")
	c.Flags().StringVar(&canaryRun, "canary-run", "", "unique run identifier for the staged canary")
	c.Flags().StringVar(&canaryScenario, "canary-scenario", "", "recorded fixture scenario for the staged canary")
	c.Flags().StringVar(&canaryOutput, "canary-output", "", "write the resulting canary evidence JSON to this file")
	c.Flags().StringSliceVar(&canarySiblings, "canary-sibling", nil, "cross-platform release binary from the same build to emit sibling canary evidence for on success")
	c.Flags().BoolVar(&controller, recoveryControllerFlag, false, "run the recovery controller of a candidate in probation")
	c.Flags().StringVar(&recoveryTarget, recoveryTargetFlag, "", "the installed executable the recovery controller supervises")
	c.Flags().DurationVar(&every, recoveryEveryFlag, recoveryEvery, "time between two recovery evaluations")
	c.Flags().DurationVar(&step, recoveryStepFlag, 0, "probation time one evaluation counts, by default that time")
	for _, hidden := range []string{recoveryControllerFlag, recoveryTargetFlag, recoveryEveryFlag, recoveryStepFlag, "canary-scenario"} {
		_ = c.Flags().MarkHidden(hidden)
	}
	c.MarkFlagsRequiredTogether(recoveryControllerFlag, recoveryTargetFlag)
	for _, flag := range []string{guideWorkCheck, "complete-probation", "rollback", syncPluginsFlag, "stage-canary", hostVersionFlag, recoveryControllerFlag, "clear-recovery-hold"} {
		c.MarkFlagsMutuallyExclusive(recoverOutdatedJudgeFlag, flag)
	}
	c.MarkFlagsMutuallyExclusive("check", "complete-probation", "rollback", syncPluginsFlag, "stage-canary")
	c.MarkFlagsMutuallyExclusive(hostVersionFlag, "complete-probation", "rollback", syncPluginsFlag, "stage-canary")
	c.MarkFlagsRequiredTogether("rollback", "rollback-reason")
	_ = c.Flags().MarkHidden(syncPluginsFlag)
	return c
}

func (a *app) runStagedCanary(ctx context.Context, cfg loopworkflow.CanaryConfig, outPath string) error {
	ev, err := runStageCanary(ctx, cfg)
	if outPath = strings.TrimSpace(outPath); outPath != "" && ev.SHA256 != "" && ev.Version != "" {
		raw, merr := json.MarshalIndent(ev, "", "  ")
		if merr != nil {
			return merr
		}
		raw = append(raw, '\n')
		if werr := os.WriteFile(outPath, raw, 0o600); werr != nil {
			return werr
		}
	}
	if ev.SHA256 != "" {
		_ = a.printJSON(ev)
	}
	return err
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
	if binaryIdentity() != "" && syncsUnder(a.explicitConfig()) && runsInstalled() {
		if err := install.RefreshSkills(a.cfg.StateDir); err != nil {
			return err
		}
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
	return sub == gate.GateCmdName || sub == gate.MergeChildCmd
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

// noteUncompared says, and logs, that candidate version goes on without a
// dry-run comparison, and why.
func (a *app) noteUncompared(w io.Writer, version, why string) {
	version = "v" + strings.TrimPrefix(version, "v")
	line := fmt.Sprintf("%s is not compared with the running release: %s; signature, canary checks and probation still apply", version, why)
	_, _ = fmt.Fprintln(w, line+".")
	if s := a.selfUpdateStore(); s != nil {
		_ = s.Record(event(state.Party{Name: selfUpdateCmdName}, verbSelfUpdateComparison, "%s", line))
	}
}

func (a *app) canaryRelease(ctx context.Context, w io.Writer, path, version string) error {
	name, why := canaryCheck(ctx, a.cfgPath, path)
	if why == "" {
		return nil
	}
	// Live budget, status and capacity failures can be transient. Only
	// the offline version probe diagnoses a broken local candidate.
	if name == hostVersionFlag && ctx.Err() == nil {
		a.skipRelease(version, why)
	}
	_, _ = fmt.Fprintf(w, "Skipping %s (%s); keeping the current binary.\n", version, why)
	return fmt.Errorf("skipping %s: %s", version, why)
}

// probationHealth distinguishes a broken offline version probe from unavailable
// live evidence. A busy state lock or budget outage must remain retryable.
func probationHealth(ctx context.Context, cfgPath, path string) error {
	name, why := canaryCheck(ctx, cfgPath, path)
	if why == "" {
		return nil
	}
	if name != hostVersionFlag || ctx.Err() != nil {
		return fmt.Errorf("%w: %s", update.ErrHealthUnproven, why)
	}
	return errors.New(why)
}

// canaryFailure runs the canary checks on the binary at path and says why the
// first failed, "" when all passed.
func canaryFailure(ctx context.Context, cfgPath, path string) string {
	_, why := canaryCheck(ctx, cfgPath, path)
	return why
}

func canaryCheck(ctx context.Context, cfgPath, path string) (string, string) {
	for _, check := range canaryChecks {
		name := strings.Join(check, " ")
		args := check
		if cfgPath != "" {
			args = append([]string{"--" + configFlag, cfgPath}, check...)
		}
		out, err := runCanaryCheck(ctx, path, args...)
		if err != nil {
			detail := oneLine(out)
			if detail == "" {
				detail = oneLine(err.Error())
			}
			return name, fmt.Sprintf("canary %s failed: %s", name, detail)
		}
	}
	return "", ""
}

// warnUpdateConfig runs before handing control to a newer binary, which may
// understand configuration that this installed version cannot read.
func (a *app) warnUpdateConfig(w io.Writer) {
	_, _ = fmt.Fprintln(w, "WARNING: configuration changes made with a newer release may be unreadable by the currently installed binary. Before changing configuration, finish the update and restart older beekeeper processes; validate configuration before reverting.")

	if err := a.loadConfig(); err != nil {
		_, _ = fmt.Fprintf(w, "WARNING: the installed binary cannot read the configuration: %v. A newer release may accept or write values this version cannot read; repair the configuration before reverting.\n", err)
	}
}

// recoverOutdatedJudge runs from the exact signed candidate previously installed
// and retained by the updater, never from the old installed recovery target.
func (a *app) recoverOutdatedJudge(ctx context.Context, target string) error {
	if err := a.loadConfig(); err != nil {
		return err
	}
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	scratch, err := os.MkdirTemp("", "beekeeper-judge-recovery-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	if err := update.RecoverOutdatedJudge(ctx, target, self, project.Version(), func(ctx context.Context, binary string) error {
		return loopworkflow.CandidateProbe(ctx, binary, "", scratch)
	}, store.RecoverVersion); err != nil {
		return err
	}
	if err := store.ResumeRecoveredWrites(project.Version()); err != nil {
		return err
	}
	if err := store.Record(event(state.Party{Name: selfUpdateCmdName}, verbRecovery, "%s promoted after candidate-owned probe; outdated judge recovery", project.Version())); err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.out, "Candidate-owned probe passed; verified candidate promoted and recovery fence completed. Existing processes move at their quiet boundaries.")
	return err
}
