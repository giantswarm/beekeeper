package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/sandbox"
)

func (a *app) sandboxCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "sandbox",
		Short: "The agent sandbox: the policy Claude Code enforces on every session's commands and file tools",
		Long: `The agent sandbox holds every Claude Code session on the machine, the
headless turns beekeeper starts and the CLI the desktop app spawns alike. Its
policy is a drop-in of Claude Code's managed settings, enforced by
Anthropic's sandbox runtime (bubblewrap on Linux, Seatbelt on macOS):

  - reads: the home directory is denied; beekeeper's binary, config and
    state, the harness's transcripts, plans, skills and plugins, git's
    config and sandbox.allowRead are re-allowed;
  - writes: the session's working directory, the temporary directory,
    beekeeper's state and sandbox.allowWrite;
  - egress: GitHub and sandbox.domains, nothing else;
  - the GitHub token (sandbox.mask): commands see a placeholder, the
    sandbox proxy puts the real one into requests to GitHub only;
  - no command leaves the sandbox, and a session where it cannot start
    does not start.

The harness's own file tools (Read, Grep, Glob, Edit, Write, NotebookEdit)
run outside the sandbox; beekeeper's PreToolUse hook holds them to the same
lists in every session the policy sets ` + sandbox.Env + ` in.`,
		Args: cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return a.loadConfig()
		},
	}
	c.AddCommand(&cobra.Command{
		Use:   "render",
		Short: "Print the policy as Claude Code settings",
		Long: `render prints the policy as the managed settings drop-in install puts in
place. The same file passed to claude --settings holds one session to it,
to try a change before installing it.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			b, err := a.sandboxPolicy().JSON()
			if err != nil {
				return err
			}
			_, err = a.out.Write(b)
			return err
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "install",
		Short: "Stage the policy and print the root command that installs it",
		Long: `install writes the policy to the state directory and prints the one root
command that copies it into Claude Code's managed settings (` + sandbox.Path() + `);
beekeeper never runs as root. Once installed, every new Claude Code session
on the machine runs in the sandbox. Exit 0 with nothing to do when the
installed policy is the current one.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			b, err := a.sandboxPolicy().JSON()
			if err != nil {
				return err
			}
			if cur, err := os.ReadFile(sandbox.Path()); err == nil && bytes.Equal(cur, b) {
				_, err = fmt.Fprintf(a.out, "the policy in %s is current\n", sandbox.Path())
				return err
			}
			staged := filepath.Join(a.cfg.StateDir, sandbox.DropIn)
			if err := os.WriteFile(staged, b, 0o600); err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "staged %s; install it as root:\n  sudo install -D -m 0644 -o root %s %s\n",
				staged, guard.ShellQuote(staged), guard.ShellQuote(sandbox.Path()))
			return err
		},
	})
	return c
}

// sandboxed is the policy that holds this session's file tools, nil when no
// sandbox holds it. Without a config only the working and temporary
// directories stay open in the home directory.
func (a *app) sandboxed(cfgErr error) *sandbox.Policy {
	if os.Getenv(sandbox.Env) == "" {
		return nil
	}
	if cfgErr != nil {
		home, _ := os.UserHomeDir()
		return &sandbox.Policy{Home: home}
	}
	p := a.sandboxPolicy()
	return &p
}

// sandboxPolicy is the agent sandbox's policy under the loaded config.
func (a *app) sandboxPolicy() sandbox.Policy {
	home, _ := os.UserHomeDir()
	exe, _ := os.Executable()
	cfgFile, _ := config.Path(a.cfgPath)
	return sandbox.New(a.cfg.Sandbox, sandbox.Paths{Home: home, ConfigFile: cfgFile, StateDir: a.cfg.StateDir, LeaseDir: a.cfg.LeaseDir, Exe: exe})
}
