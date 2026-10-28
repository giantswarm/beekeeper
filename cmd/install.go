package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/install"
	"github.com/giantswarm/beekeeper/internal/proc"
)

func (a *app) installCmd() *cobra.Command {
	var e install.Env
	var binary string
	c := &cobra.Command{
		Use:   "install",
		Short: "Put the hooks, the standby service, the keeper, the memory guard and a starter config in place",
		Long: `Install puts beekeeper in place for this user, with the absolute path of the
binary it runs from (or --binary):

  - the PreToolUse and PermissionRequest hooks in Claude Code's user
    settings ($CLAUDE_CONFIG_DIR, else ~/.claude/settings.json), and when
    agy is installed the PreInvocation and PreToolUse hooks under
    "beekeeper" in ~/.gemini/config/hooks.json and the embedded skills in
    <stateDir>/skills registered under "entries" in
    ~/.gemini/config/skills.json, merged into what is there;
  - the standby service (beekeeper watch --standby, with --notify when
    watch.notify is true; off by default): a systemd user unit on Linux, a
    launch agent on macOS, enabled and started;
  - with systemd, the agent sandbox's broker (beekeeper sandbox broker):
    beekeeper-sandbox.service, enabled and started;
  - with systemd and teleport.proxy set, the Teleport login's keeper:
    beekeeper-teleport.timer, enabled and started, and the service it
    starts every teleport.every (beekeeper teleport renew --keeper);
  - with systemd, the memory guard sized to the machine's RAM: memcap.slice,
    which beekeeper run's capped commands sit in, with their CPU budget
    (memcap.cpuQuota, memcap.cpuWeight), and a drop-in for the Claude
    Desktop scope that runs;
  - a starter config, the example configuration with every key commented
    out, when no config exists.

Run from the binary it installs, it also brings an installed beekeeper
plugin whose skills differ from the binary's up to its marketplace's
latest, as self-update does.

A file that is already what install writes stays as it is. One an earlier
install wrote, a unit that runs this binary and a hook entry that runs this
binary's hook command (with an older matcher, say) are updated in place,
never added twice; one it finds differing that is none of these it keeps:
install changes nothing it did not put there, and a second run changes
nothing, so running it after every update is safe.

A running beekeeper watch (the standby service's included) and a reopen
waiting to show a session that still run the binary an update replaced at
this path restart on the installed one once, each at its next quiet
moment: a watch after its current poll, once every run it started ended,
a reopen during its wait for the person. Each is named (restart). The
broker that runs the binary replaced at this path is restarted onto it,
its vault session dropped, unless it serves calls: then the restart still
due is named (due), as self-update does. What it wrote is
recorded in install.json in the state directory, for uninstall.
Without a service manager (Linux without systemd) it writes the hooks and
the config and says the service is not available.`,
		Args: cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return a.loadConfig()
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.installEnv(&e, binary); err != nil {
				return err
			}
			if err := install.Install(cmd.Context(), e); err != nil {
				return err
			}
			if t, err := proc.Read(); err == nil {
				if _, err := a.restartReplaced(e.Out, e.Exe, t, e.DryRun); err != nil {
					return err
				}
				if fi, err := os.Stat(e.Exe); err == nil {
					restartBrokerOn(cmd.Context(), e.Out, e.Exe, fi.ModTime(), t, e.DryRun)
				}
			}
			if binary == "" && !e.DryRun {
				syncPlugins(cmd.Context(), a.out, false)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&e.DryRun, "dry-run", false, "print every step and take none")
	c.Flags().StringVar(&binary, "binary", "", "the beekeeper binary the hooks and the service run (default: this one)")
	return c
}

func (a *app) uninstallCmd() *cobra.Command {
	var e install.Env
	var purge bool
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove what install put in place",
		Long: `Uninstall stops the units install started and removes the files,
directories and hooks install wrote, as install.json in the state directory
records them; a file changed since install wrote it stays. A unit path that
is a symlink (into dotfiles) stays as the person's link: a target an earlier
install wrote through it is removed, its unit stopped, never disabled. The
config and the state stay too, unless --purge.`,
		Args: cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return a.loadConfig()
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.installEnv(&e, ""); err != nil {
				return err
			}
			return install.Uninstall(cmd.Context(), e, purge)
		},
	}
	c.Flags().BoolVar(&e.DryRun, "dry-run", false, "print every step and take none")
	c.Flags().BoolVar(&purge, "purge", false, "remove the config file and the state directory too")
	return c
}

// installEnv fills e with this machine and binary, the running one when
// binary is empty.
func (a *app) installEnv(e *install.Env, binary string) error {
	var err error
	if binary == "" {
		if binary, err = os.Executable(); err != nil {
			return err
		}
	}
	if e.Exe, err = filepath.Abs(binary); err != nil {
		return err
	}
	if e.Spec.Home, err = os.UserHomeDir(); err != nil {
		return err
	}
	if e.Spec.ConfigDir, err = os.UserConfigDir(); err != nil {
		return err
	}
	if e.Config, err = config.Path(a.cfgPath); err != nil {
		return err
	}
	claudeDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeDir == "" {
		claudeDir = filepath.Join(e.Spec.Home, ".claude")
	}
	e.Settings = filepath.Join(claudeDir, "settings.json")
	agyInstalled := hasAgy()
	e.AgyHooks = agyHooksPath(e.Spec.Home, agyInstalled)
	e.AgySkills = agySkillsPath(e.Spec.Home, agyInstalled)
	e.StateDir = a.cfg.StateDir
	e.Spec.Exe = e.Exe
	if m, err := plat.Machine.Mem(); err == nil {
		e.Spec.RAMMiB, e.Spec.SwapMiB = m.TotalMiB, m.SwapTotalMiB
	}
	cpu := a.memcapCPU()
	e.Spec.CPUQuota, e.Spec.CPUWeight = cpu.CPUQuota, cpu.CPUWeight
	if s, err := plat.Machine.DesktopScope(); err == nil && s != nil {
		e.Spec.DesktopScope = filepath.Base(s.Path)
	}
	if a.cfg.Teleport.Enabled() {
		e.Spec.TeleportEvery = a.cfg.Teleport.Every.Duration
	}
	e.Spec.Notify = a.cfg.Watch.Notify
	e.Setup = plat.Setup
	e.Run = runCommand
	e.Out = a.out
	return nil
}

// hasAgy reports whether the agy CLI is installed on PATH.
func hasAgy() bool {
	_, err := exec.LookPath("agy")
	return err == nil
}

// agyHooksPath returns ~/.gemini/config/hooks.json when agy is installed or
// the hooks file already exists, "" otherwise.
func agyHooksPath(home string, agyInstalled bool) string {
	return agyConfigPath(home, "hooks.json", agyInstalled)
}

// agySkillsPath returns ~/.gemini/config/skills.json when agy is installed or
// the skills manifest already exists, "" otherwise.
func agySkillsPath(home string, agyInstalled bool) string {
	return agyConfigPath(home, "skills.json", agyInstalled)
}

func agyConfigPath(home, name string, agyInstalled bool) string {
	if home == "" {
		return ""
	}
	p := filepath.Join(home, ".gemini", "config", name)
	if _, err := os.Lstat(p); err == nil || agyInstalled {
		return p
	}
	return ""
}

// runCommand runs one service manager command, its output in the error.
func runCommand(ctx context.Context, argv []string) error {
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput() //nolint:gosec // the platform's own service manager commands
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
