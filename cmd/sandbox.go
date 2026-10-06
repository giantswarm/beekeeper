package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/omp"
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
  - denied inside them, for reading and writing: the value scanner's key
    and index (scan/ in the state directory);
  - egress: through the broker's egress proxy alone (sandbox.proxyPort),
    to GitHub and sandbox.domains, nothing else; loopback by port only
    (127.0.0.1:<port>, a kind lab's API server, which a held lab's
    kubeconfig reaches through the sandbox's SOCKS proxy);
  - the GitHub token: never in the sandbox. The egress proxy terminates
    TLS for GitHub's hosts with a CA of its own, which the policy has the
    sandbox's clients trust, and sets the Authorization header itself;
  - no command leaves the sandbox, and a session where it cannot start
    does not start.

The harness's own file tools (Read, Grep, Glob, Edit, Write, NotebookEdit)
run outside the sandbox; beekeeper's PreToolUse hook holds them to the same
lists in every session the policy sets ` + sandbox.Env + ` in.

The sandbox blocks every Unix socket on Linux, the user bus included, so a
sandboxed beekeeper run asks the host's broker (beekeeper sandbox broker,
the unit beekeeper-sandbox.service) for its capped scope; the command
stays in the sandbox. What a role needs from the host runs there through
the same broker, as the asking session: the gated devctl commands, agents
start, wake and resume, the watch, and a held lab's lease up and lease
down. A sandboxed git signs through the broker as well.`,
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
		Short: "Stage the policy and print the root commands that install it",
		Long: `install writes the policy to the state directory and prints the root
commands that copy it into Claude Code's managed settings (` + sandbox.Path() + `);
beekeeper never runs as root. On a fresh machine they create the managed
settings file (an empty ` + "`{}`" + `) and its drop-in directory first: the sandbox
mounts both read-only and cannot create them, so a session held by
claude --settings needs them too. Once installed, every new Claude Code
session on the machine runs in the sandbox. Exit 0 with nothing to do when
the installed policy is the current one.

install creates the denied paths' mount points (scan/ in the state
directory), which the sandbox cannot create; the broker does too at its
start. It refuses a writable path inside a readable one (a
sandbox.allowWrite under a sandbox.allowRead) and names both: the sandbox
mounts the readable parent read-only over the writable child.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p := a.sandboxPolicy()
			if err := p.Nested(); err != nil {
				return refused("%s", err)
			}
			if err := p.MountPoints(); err != nil {
				return err
			}
			b, err := p.JSON()
			if err != nil {
				return err
			}
			steps, err := sandboxInstallSteps(sandbox.ManagedDir(), a.cfg.StateDir, b)
			if err != nil || len(steps) == 0 {
				if err == nil {
					_, err = fmt.Fprintf(a.out, "the policy in %s is current\n", sandbox.Path())
				}
				return err
			}
			_, err = fmt.Fprintf(a.out, "staged in %s; install as root:\n  %s\n", a.cfg.StateDir, strings.Join(steps, "\n  "))
			return err
		},
	})
	c.AddCommand(a.sandboxBrokerCmd(), sandboxScopeCmd(), a.sandboxGPGCmd())
	return c
}

// emptyManagedSettings is the managed settings file a fresh machine gets.
const emptyManagedSettings = "{}\n"

// sandboxInstallSteps stages what the managed settings in dir lack for the
// policy b in stateDir and returns the root commands that put it in place:
// the managed settings file and the drop-in directory, which the sandbox
// mounts read-only and cannot create, and the policy's drop-in. None when
// all of it is current.
func sandboxInstallSteps(dir, stateDir string, b []byte) ([]string, error) {
	settings, dropIns := filepath.Join(dir, "managed-settings.json"), filepath.Join(dir, "managed-settings.d")
	path := filepath.Join(dropIns, sandbox.DropIn)
	var steps []string
	if _, err := os.Stat(settings); errors.Is(err, os.ErrNotExist) {
		staged := filepath.Join(stateDir, "managed-settings.json")
		if err := os.WriteFile(staged, []byte(emptyManagedSettings), 0o600); err != nil {
			return nil, err
		}
		steps = append(steps, "sudo install -d -m 0755 "+guard.ShellQuote(dir),
			"sudo install -m 0644 -o root "+guard.ShellQuote(staged)+" "+guard.ShellQuote(settings))
	}
	if _, err := os.Stat(dropIns); errors.Is(err, os.ErrNotExist) {
		steps = append(steps, "sudo install -d -m 0755 "+guard.ShellQuote(dropIns))
	}
	if cur, err := os.ReadFile(path); err != nil || !bytes.Equal(cur, b) { //nolint:gosec // the policy's own drop-in
		staged := filepath.Join(stateDir, sandbox.DropIn)
		if err := os.WriteFile(staged, b, 0o600); err != nil {
			return nil, err
		}
		steps = append(steps, "sudo install -m 0644 -o root "+guard.ShellQuote(staged)+" "+guard.ShellQuote(path))
	}
	return steps, nil
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
a session can cap itself and nothing else.

A sandboxed beekeeper secret call (compare, fingerprint, copy, set, rotate)
runs here too, where sops and op reach their keys: as the asking session, in
its working directory, with its files held to the sandbox's lists, and its
output, never a value, goes back through the spool. A consumer (copy --), the
vault's setup and import run on the host only, by the person.

A sandboxed beekeeper lease kubeconfig runs here as well, where kind reaches
the container runtime: for the session that holds the lab's lease only.

The broker is the sandbox's egress proxy, on 127.0.0.1:<sandbox.proxyPort>
for its own user only: Claude Code bridges the sandbox's HTTP and SOCKS5
proxies to it. It reaches GitHub and sandbox.domains and refuses the rest,
and never a loopback or link-local address by name. Every five minutes it
reads devctl's App user token (devctl auth exec, sandbox.devctl) into its
memory, and it terminates TLS for github.com, api.github.com and
uploads.github.com with a CA it makes at start, its key in memory alone, to
set the Authorization header of each request; bodies pass untouched. The
CA, a bundle of the system's roots and the CA, and gh's login (a fixed
word, no token) go to $XDG_RUNTIME_DIR/beekeeper/egress.

A sandboxed session's gated devctl command (pr merge, pr wait, release
promote, release wait, rollout wait) runs here as beekeeper gate, as the
session and in its working directory, where devctl reads its keychain and
the gate starts its units; its output and exit code go back through the
spool once it ends.

A sandboxed session's agents start, wake and resume run here too, where
the user's service manager starts the agent's unit and the desktop imports
it: the start's brief and directory held to the session's own lists. Its
watch runs here in a scope of its own, where it reads the installations
through the person's kubeconfig and Teleport login, neither of which the
sandbox opens; and so do a held lab's lease up and lease down, where kind
reaches the container runtime. Their output streams back while they run,
and a call whose sandboxed command ended is ended with it.

A sandboxed git signs its commits and tags here: its gpg.program sends the
payload, and the broker signs it with the person's gpg, where gpg reaches
its agent, and answers the signature and gpg's status lines. It signs
git's signing call only; the key and the agent stay out of the sandbox.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if os.Getenv(sandbox.Env) != "" {
				return refused("the broker runs on the host: this session is in the agent sandbox")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			// a state directory made after the install lacks them as well
			if err := a.sandboxPolicy().MountPoints(); err != nil {
				return err
			}
			dir := sandbox.SpoolDir(a.cfg.StateDir)
			_, _ = fmt.Fprintf(a.out, "serving %s\n", dir)
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			// the calls it runs read the broker's configuration, not the
			// default one
			if a.cfgPath != "" {
				abs, err := filepath.Abs(a.cfgPath)
				if err != nil {
					return err
				}
				if err := os.Setenv("BEEKEEPER_CONFIG", abs); err != nil {
					return err
				}
			}
			serveEgress, err := a.listenEgress(ctx)
			if err != nil {
				return err
			}
			egress := make(chan error, 1)
			go func() { egress <- serveEgress() }()
			keeper, err := a.keepVault(ctx)
			if err != nil {
				return err
			}
			secretCall := func(env []string) sandbox.Handler {
				return brokeredCall(exe, "/proc", brokeredCallTimeout, env, brokeredSecretArgv)
			}
			spool := make(chan error, 1)
			go func() {
				spool <- sandbox.Serve(ctx, dir, "/proc", brokerTick, brokered(brokeredCap(plat.Capper), map[string]sandbox.Handler{
					sandbox.OpSecret:     a.brokeredVault(keeper, secretCall),
					sandbox.OpVault:      brokeredVaultState(keeper),
					sandbox.OpKubeconfig: brokeredCall(exe, "/proc", brokeredCallTimeout, nil, brokeredKubeconfigArgv),
					sandbox.OpGate:       brokeredCall(exe, "/proc", gateBrokeredTimeout, devctlPath(a.cfg.Sandbox.Devctl), brokeredGateArgv),
					sandbox.OpAgents: brokeredAgents(func(env []string) sandbox.Handler {
						return brokeredCall(exe, "/proc", agentsBrokeredTimeout, env, brokeredAgentsArgv)
					}),
					sandbox.OpWatch: brokeredCall(exe, "/proc", 0, nil, brokeredWatchArgv),
					sandbox.OpLab:   brokeredCall(exe, "/proc", labBrokeredTimeout, nil, brokeredLabArgv),
					sandbox.OpSign:  brokeredSign(hostGPG(ctx)),
				}))
			}()
			// either one ending ends the broker, which its unit restarts
			select {
			case err := <-spool:
				return err
			case err := <-egress:
				if err == nil && ctx.Err() == nil {
					err = errors.New("the egress proxy stopped")
				}
				return err
			}
		},
	}
}

var (
	// brokeredSlice is memcap.slice or a build slot's slice under it.
	brokeredSlice = regexp.MustCompile(`^memcap(-slot[0-9]+_([0-9a-f]{8}|test))?\.slice$`)
	// brokeredUnit is a capped run's scope name.
	brokeredUnit = regexp.MustCompile(`^` + guard.ScopePrefix + `(test-)?[0-9]+-[0-9]{6}$`)
)

// brokered answers a sandboxed session's requests: the operations of calls
// with their handler, the rest with capRun.
func brokered(capRun func(int, sandbox.Request) error, calls map[string]sandbox.Handler) sandbox.Handler {
	return func(ctx context.Context, pid int, req sandbox.Request) (sandbox.Reply, error) {
		if h, ok := calls[req.Op]; ok {
			return h(ctx, pid, req)
		}
		return sandbox.Reply{}, capRun(pid, req)
	}
}

// brokeredCap acts on a sandboxed run's request with the host's capper:
// memcap's slices and scopes only, sizes as beekeeper run takes them.
func brokeredCap(c platform.Capper) func(int, sandbox.Request) error {
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

// brokeredSecretOps are the beekeeper secret subcommands a sandboxed
// session runs through the broker. copy's consumer form is refused with
// them: its consumer would run on the host, outside the sandbox.
var brokeredSecretOps = []string{"compare", "fingerprint", "copy", "set", "rotate"}

// callerEnv are the variables that name the calling session, the only part
// of its environment a brokered call takes over.
var callerEnv = []string{"CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_HOST_SESSION_ID", "CLAUDE_CODE_SESSION_NAME", "CLAUDE_CONFIG_DIR", omp.EnvAgent, omp.EnvName}

// brokeredCallTimeout bounds a brokered call: op answers within a minute, a
// rotation writes several files.
const brokeredCallTimeout = 10 * time.Minute

// brokeredSecretArgs refuses a secret call the broker does not run: one
// outside brokeredSecretOps, another caller or config, or for a sandboxed
// requester a consumer, which would run outside its sandbox.
func brokeredSecretArgs(args []string, inSandbox bool) error {
	if len(args) == 0 || !slices.Contains(brokeredSecretOps, args[0]) {
		return fmt.Errorf("the sandbox broker runs beekeeper secret %s only", strings.Join(brokeredSecretOps, ", "))
	}
	for _, a := range args[1:] {
		if a == "--" && inSandbox {
			return errors.New("a consumer runs on the host only: copy or set with -- is not brokered")
		}
	}
	return brokeredFlags(args[1:])
}

// brokeredFlags refuses the flags that would make a brokered call another
// caller's or another config's, up to the end of the flags ("--").
func brokeredFlags(args []string) error {
	for _, a := range args {
		if a == "--" {
			return nil
		}
		for _, f := range []string{"--as", "--config"} {
			if a == f || strings.HasPrefix(a, f+"=") {
				return fmt.Errorf("%s: a brokered call runs as its session, under the broker's config", f)
			}
		}
	}
	return nil
}

// brokeredSecretArgv is the command line of a brokered secret call.
func brokeredSecretArgv(req sandbox.Request, inSandbox bool) ([]string, error) {
	if err := brokeredSecretArgs(req.Args, inSandbox); err != nil {
		return nil, err
	}
	return append([]string{"secret"}, req.Args...), nil
}

// resourceName is a lease's resource name.
var resourceName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// brokeredKubeconfigArgv is the command line of a brokered lease
// kubeconfig.
func brokeredKubeconfigArgv(req sandbox.Request, _ bool) ([]string, error) {
	if !resourceName.MatchString(req.Resource) {
		return nil, fmt.Errorf("%q is not a resource", req.Resource)
	}
	return []string{leaseName, "kubeconfig", req.Resource}, nil
}

// brokeredStopDelay is how long a brokered call ended early (its timeout, its
// streamed requester gone) has to exit after SIGTERM before it is killed.
const brokeredStopDelay = 10 * time.Second

// brokeredCall runs a session's beekeeper call, argv's for its request and
// whether the requester is in the sandbox, with exe on the host for up to
// timeout (0: as long as it runs): as the session that holds the request, in its working directory,
// with env on top of the broker's environment (whose vault credentials it
// drops), marked sandbox.Brokered so that it asks no broker, and for a
// sandboxed requester sandbox.Env, so that the call holds itself to the
// sandbox's lists.
func brokeredCall(exe, procDir string, timeout time.Duration, env []string, argv func(sandbox.Request, bool) ([]string, error)) sandbox.Handler {
	return func(ctx context.Context, pid int, req sandbox.Request) (sandbox.Reply, error) {
		inSandbox, err := sandbox.Sandboxed(procDir, pid)
		if err != nil {
			return sandbox.Reply{}, fmt.Errorf("the requester: %w", err)
		}
		args, err := argv(req, inSandbox)
		if err != nil {
			return sandbox.Reply{}, err
		}
		cwd, caller, err := sandbox.Origin(procDir, pid, callerEnv)
		if err != nil {
			return sandbox.Reply{}, fmt.Errorf("the requester: %w", err)
		}
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		c := exec.CommandContext(ctx, exe, args...) //nolint:gosec // this binary, the arguments checked by argv
		// ended as a terminal ends it: a watch says nothing more, a start
		// leaves no half-written state
		c.Cancel = func() error { return c.Process.Signal(syscall.SIGTERM) }
		c.WaitDelay = brokeredStopDelay
		c.Dir = cwd
		c.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
			k, _, _ := strings.Cut(kv, "=")
			return k == sandbox.Env || k == sandbox.Brokered || k == stateFromEnv || slices.Contains(callerEnv, k) || guard.VaultVar.MatchString(k)
		})
		c.Env = append(append(append(c.Env, env...), caller...), sandbox.Brokered+"=1")
		if inSandbox {
			c.Env = append(c.Env, sandbox.Env+"=1")
		}
		var out, errOut bytes.Buffer
		c.Stdout, c.Stderr = &out, &errOut
		if w := req.Output(); w != nil {
			c.Stdout = w
		}
		err = c.Run()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			return sandbox.Reply{}, err
		}
		return sandbox.Reply{Out: out.String(), Err: errOut.String(), Code: c.ProcessState.ExitCode()}, nil
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
	return sandbox.New(a.cfg.Sandbox, sandbox.Paths{Home: home, ConfigFile: cfgFile, StateDir: a.cfg.StateDir, LeaseDir: a.cfg.LeaseDir, SlotDir: a.cfg.Memcap.SlotDir, Exe: exe, ScanDir: a.scanDir(),
		RuntimeDir: os.Getenv("XDG_RUNTIME_DIR")})
}
