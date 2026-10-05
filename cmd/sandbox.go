package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/platform"
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
    beekeeper's state, the build slots and sandbox.allowWrite;
  - egress: GitHub and sandbox.domains, nothing else;
  - the GitHub token (sandbox.mask): commands see a placeholder, the
    sandbox proxy puts the real one into requests to GitHub only;
  - no command leaves the sandbox, and a session where it cannot start
    does not start.

The harness's own file tools (Read, Grep, Glob, Edit, Write, NotebookEdit)
run outside the sandbox; beekeeper's PreToolUse hook holds them to the same
lists in every session the policy sets ` + sandbox.Env + ` in.

The sandbox blocks every Unix socket on Linux, the user bus included, so a
sandboxed beekeeper run asks the host's broker (beekeeper sandbox broker,
the unit beekeeper-sandbox.service) for its capped scope; the command
stays in the sandbox.`,
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
	c.AddCommand(a.sandboxBrokerCmd(), sandboxScopeCmd())
	return c
}

// brokerTick is how often the broker reads its spool.
const brokerTick = 50 * time.Millisecond

func (a *app) sandboxBrokerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "broker",
		Short: "Serve the sandboxed sessions' capped runs on the host",
		Long: `broker runs on the host, outside the sandbox, as the user unit
beekeeper-sandbox.service that beekeeper install puts in place. The sandbox
blocks every Unix socket on Linux, the user bus included, so a sandboxed
beekeeper run cannot start its scope: it asks the broker instead, through
request files in the spool under beekeeper's state directory (` + "`<stateDir>/sandbox`" + `),
which the policy lets sessions write.

The broker caps a build slot's slice and moves the asking process into its
memcap scope, then the command runs there, still in the sandbox. It answers
each request as the one process of this user that holds it open, never a
process the request names, and only for memcap's own slices and scopes, so
a session can cap itself and nothing else.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if os.Getenv(sandbox.Env) != "" {
				return refused("the broker runs on the host: this session is in the agent sandbox")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			dir := sandbox.SpoolDir(a.cfg.StateDir)
			_, _ = fmt.Fprintf(a.out, "serving %s\n", dir)
			return sandbox.Serve(ctx, dir, "/proc", brokerTick, brokeredCap(plat.Capper))
		},
	}
}

var (
	// brokeredSlice is memcap.slice or a build slot's slice under it.
	brokeredSlice = regexp.MustCompile(`^memcap(-slot[0-9]+_([0-9a-f]{8}|test))?\.slice$`)
	// brokeredUnit is a capped run's scope name.
	brokeredUnit = regexp.MustCompile(`^` + guard.ScopePrefix + `(test-)?[0-9]+-[0-9]{6}$`)
)

// brokeredCap acts on a sandboxed run's request with the host's capper:
// memcap's slices and scopes only, sizes as beekeeper run takes them.
func brokeredCap(c platform.Capper) sandbox.Handler {
	return func(pid int, req sandbox.Request) error {
		if req.Op == sandbox.OpPing {
			return nil
		}
		if !brokeredSlice.MatchString(req.Slice) {
			return fmt.Errorf("slice %q is not a memcap slice", req.Slice)
		}
		for _, s := range []string{req.Max, req.Swap} {
			if _, err := guard.ParseSize(s); err != nil && s != "infinity" {
				return err
			}
		}
		cp := platform.Cap{Max: req.Max, Swap: req.Swap, Slice: req.Slice}
		switch req.Op {
		case sandbox.OpCapSlot:
			return c.CapSlot(cp)
		case sandbox.OpScope:
			if !brokeredUnit.MatchString(req.Unit) {
				return fmt.Errorf("scope %q is not a capped run's", req.Unit)
			}
			return c.Adopt(pid, req.Unit, cp)
		}
		return fmt.Errorf("unknown request %q", req.Op)
	}
}

// sandboxScopeCmd is the sandboxed side of a capped run: it asks the broker
// to move it into the run's scope and becomes the command there.
func sandboxScopeCmd() *cobra.Command {
	var spool, unit string
	var cp platform.Cap
	c := &cobra.Command{
		Use:    "scope --spool DIR --unit NAME --slice SLICE --max SIZE --swap SIZE -- command [args...]",
		Short:  "Enter a capped run's scope through the broker and exec the command (beekeeper run's, in the sandbox)",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, argv []string) error {
			if err := sandbox.EnterScope(spool, unit, cp); err != nil {
				return err
			}
			path, err := exec.LookPath(argv[0])
			if err != nil {
				return &exitError{code: guard.ExitNotFound, msg: err.Error()}
			}
			return syscall.Exec(path, argv, os.Environ()) //nolint:gosec // running the caller's command is the purpose
		},
	}
	c.Flags().SetInterspersed(false)
	c.Flags().StringVar(&spool, "spool", "", "the broker's spool")
	c.Flags().StringVar(&unit, "unit", "", "the scope's name")
	c.Flags().StringVar(&cp.Slice, "slice", "", "the slot's slice")
	c.Flags().StringVar(&cp.Max, "max", "", "the scope's MemoryMax")
	c.Flags().StringVar(&cp.Swap, "swap", "0", "the scope's MemorySwapMax")
	_ = c.MarkFlagRequired("spool")
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
	return sandbox.New(a.cfg.Sandbox, sandbox.Paths{Home: home, ConfigFile: cfgFile, StateDir: a.cfg.StateDir, LeaseDir: a.cfg.LeaseDir, SlotDir: a.cfg.Memcap.SlotDir, Exe: exe})
}
