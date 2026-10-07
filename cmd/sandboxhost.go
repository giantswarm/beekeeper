package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/sandbox"
)

// The roles' host commands: what a supervisor or a worker needs from the
// host, which the agent sandbox closes, runs through the broker as the
// asking session (brokeredCall), streamed back while it runs.

const (
	// agentsBrokeredTimeout bounds a brokered agents start, wake or resume:
	// a start waits for the first reply and the desktop's import.
	agentsBrokeredTimeout = 30 * time.Minute
	// labBrokeredTimeout bounds a brokered lab creation or teardown.
	labBrokeredTimeout = time.Hour
)

// The agents subcommands a sandboxed session runs through the broker: those
// that start the agent's unit.
const (
	agentStartName  = "start"
	agentWakeName   = "wake"
	agentResumeName = "resume"
)

var brokeredAgentsOps = []string{agentStartName, agentWakeName, agentResumeName}

// The lab operations of lease up and lease down, and the lease command's
// name.
const (
	labUp     = "up"
	labDown   = "down"
	leaseName = "lease"
)

var labOps = []string{labUp, labDown}

// inSandbox reports whether this process runs in the agent sandbox and is
// not the broker's call itself.
func inSandbox() bool {
	return os.Getenv(sandbox.Env) != "" && os.Getenv(sandbox.Brokered) == ""
}

// onHost makes c run through the host's broker (op) when it is called in
// the agent sandbox, streamed for up to timeout (0: as long as it runs),
// and as written everywhere else.
func (a *app) onHost(c *cobra.Command, op string, timeout time.Duration) *cobra.Command {
	run := c.RunE
	c.RunE = func(cmd *cobra.Command, args []string) error {
		if !inSandbox() {
			return run(cmd, args)
		}
		if op != sandbox.OpAgents {
			argv, err := brokerArgv(cmd, args)
			if err != nil {
				return err
			}
			return a.brokeredAnswer(sandbox.Request{Op: op, Args: argv}, timeout, true)
		}
		return a.agentsOnHost(cmd, args, timeout)
	}
	return c
}

// agentsOnHost runs an agents start, wake or resume through the host's
// broker, streamed for up to timeout, its state the scratch configuration's
// when the call names one.
func (a *app) agentsOnHost(cmd *cobra.Command, args []string, timeout time.Duration) error {
	argv, err := a.scratchArgv(cmd, args)
	if err != nil {
		return err
	}
	dir, err := a.hostSpool()
	if err != nil {
		return err
	}
	return a.brokeredAnswerIn(dir, sandbox.Request{Op: sandbox.OpAgents, Args: argv}, timeout, true)
}

// brokerArgv is the command line of cmd's call for the broker: its name,
// the flags set on it, then its arguments after "--", so that none reads
// as a flag.
func brokerArgv(cmd *cobra.Command, args []string) ([]string, error) {
	argv, err := brokerFlags(cmd)
	if err != nil {
		return nil, err
	}
	return append(append(argv, "--"), args...), nil
}

// scratchArgv is brokerArgv for an agents start, wake or resume, which may
// keep its state in a scratch configuration's: the --config the caller
// named (or $BEEKEEPER_STATE_FROM it runs under) goes to the broker as
// --config=<absolute path>, which brokeredAgents takes for the scratch
// state. A broker without it refuses the flag, so no start of a scratch
// configuration lands in the host's state.
func (a *app) scratchArgv(cmd *cobra.Command, args []string) ([]string, error) {
	scratch := a.cfgPath
	if scratch == "" {
		scratch = os.Getenv(stateFromEnv)
	}
	argv := []string{cmd.Name()}
	var err error
	cmd.Flags().Visit(func(f *pflag.Flag) {
		switch f.Name {
		case "as":
			err = refused("--as: a brokered call runs as this session")
		case configFlag:
		default:
			argv = append(argv, "--"+f.Name+"="+f.Value.String())
		}
	})
	if err != nil {
		return nil, err
	}
	if scratch != "" {
		abs, err := filepath.Abs(scratch)
		if err != nil {
			return nil, err
		}
		argv = append(argv, "--config="+abs)
	}
	return append(append(argv, "--"), args...), nil
}

// hostSpool is the host broker's spool: the state directory of the host's
// configuration ($BEEKEEPER_CONFIG or the default one), never the scratch
// one a call names.
func (a *app) hostSpool() (string, error) {
	if a.cfgPath == "" && os.Getenv(stateFromEnv) == "" {
		return sandbox.SpoolDir(a.cfg.StateDir), nil
	}
	path, err := config.Path("")
	if err != nil {
		return "", err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return "", err
	}
	return sandbox.SpoolDir(cfg.StateDir), nil
}

// brokerFlags is cmd's name and the flags set on it, for a brokered call.
// --as and --config are refused: a brokered call runs as its session,
// under the host's config.
func brokerFlags(cmd *cobra.Command) ([]string, error) {
	argv := []string{cmd.Name()}
	var err error
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if f.Name == "as" || f.Name == configFlag {
			err = refused("--%s: a brokered call runs as this session, under the host's config", f.Name)
		}
		argv = append(argv, "--"+f.Name+"="+f.Value.String())
	})
	return argv, err
}

// brokeredAnswer asks the host's broker for req, waiting up to timeout (0:
// as long as the call runs), and passes on its output, streamed while it
// runs with stream, and its exit code.
func (a *app) brokeredAnswer(req sandbox.Request, timeout time.Duration, stream bool) error {
	return a.brokeredAnswerIn(sandbox.SpoolDir(a.cfg.StateDir), req, timeout, stream)
}

// brokeredAnswerIn is brokeredAnswer from the broker serving dir.
func (a *app) brokeredAnswerIn(dir string, req sandbox.Request, timeout time.Duration, stream bool) error {
	if !(sandbox.Capper{Dir: dir}).Available() {
		return refused("no sandbox broker answers in %s: beekeeper-sandbox.service on the host runs this for the sandbox (beekeeper install); agents start, wake and resume name a scratch state with --config", dir)
	}
	call, out := sandbox.Call, a.out
	if stream {
		call = func(dir string, req sandbox.Request, timeout time.Duration) (sandbox.Reply, error) {
			return sandbox.Stream(dir, req, timeout, a.out)
		}
		out = io.Discard
	}
	r, err := call(dir, req, timeout)
	if err != nil {
		return refused("%v", err)
	}
	return passReply(r, out)
}

// passReply passes a brokered call's output to out, its errors to stderr,
// and its exit code on.
func passReply(r sandbox.Reply, out io.Writer) error {
	if _, err := io.WriteString(out, r.Out); err != nil {
		return err
	}
	_, _ = io.WriteString(os.Stderr, r.Err)
	if r.Code != 0 {
		return &exitError{code: r.Code}
	}
	return nil
}

// brokeredSub checks a brokered call's arguments, the subcommand first:
// one of subs, no --as or --config.
func brokeredSub(args []string, what string, subs ...string) error {
	if len(args) == 0 || !slices.Contains(subs, args[0]) {
		return fmt.Errorf("the sandbox broker runs beekeeper %s %s only", what, strings.Join(subs, ", "))
	}
	return brokeredFlags(args[1:])
}

// brokeredAgents runs a brokered agents start, wake or resume with call: a
// scratch configuration its flags name (--config=<absolute path>, from
// scratchArgv) leaves them and gives the call its state through
// $BEEKEEPER_STATE_FROM, held to the session's lists (stateFrom). The rest
// of the scratch configuration never reaches the host: its commands would
// run outside the sandbox.
func brokeredAgents(call func(env []string) sandbox.Handler) sandbox.Handler {
	return func(ctx context.Context, pid int, req sandbox.Request) (sandbox.Reply, error) {
		args, scratch, err := scratchConfig(req.Args)
		if err != nil {
			return sandbox.Reply{}, err
		}
		var env []string
		if scratch != "" {
			env = []string{stateFromEnv + "=" + scratch}
		}
		req.Args = args
		return call(env)(ctx, pid, req)
	}
}

// scratchConfig takes the scratch configuration a brokered agents call
// names out of its flags: the arguments without it, and its path.
func scratchConfig(args []string) ([]string, string, error) {
	var scratch string
	out := make([]string, 0, len(args))
	for i, arg := range args {
		if arg == "--" {
			return append(out, args[i:]...), scratch, nil
		}
		if v, ok := strings.CutPrefix(arg, "--config="); ok {
			if !filepath.IsAbs(v) {
				return nil, "", fmt.Errorf("--config=%s: a scratch configuration is named by its absolute path", v)
			}
			scratch = v
			continue
		}
		out = append(out, arg)
	}
	return out, scratch, nil
}

// brokeredAgentsArgv is the command line of a brokered agents start, wake
// or resume.
func brokeredAgentsArgv(req sandbox.Request, _ bool) ([]string, error) {
	if err := brokeredSub(req.Args, agentsName, brokeredAgentsOps...); err != nil {
		return nil, err
	}
	return append([]string{agentsName}, req.Args...), nil
}

// brokeredWatchArgv is the command line of a brokered watch.
func brokeredWatchArgv(req sandbox.Request, _ bool) ([]string, error) {
	if err := brokeredSub(req.Args, "watch", "watch"); err != nil {
		return nil, err
	}
	return req.Args, nil
}

// brokeredPersonArgv is the command line of a brokered person.
func brokeredPersonArgv(req sandbox.Request, _ bool) ([]string, error) {
	if err := brokeredSub(req.Args, personName, personName); err != nil {
		return nil, err
	}
	return req.Args, nil
}

// brokeredLabArgv is the command line of a brokered lab creation or
// teardown: lease up or lease down of the lab.
func brokeredLabArgv(req sandbox.Request, _ bool) ([]string, error) {
	if len(req.Args) != 1 || !slices.Contains(labOps, req.Args[0]) {
		return nil, errors.New("the sandbox broker runs beekeeper lease up and lease down only")
	}
	if !resourceName.MatchString(req.Resource) {
		return nil, fmt.Errorf("%q is not a resource", req.Resource)
	}
	return []string{leaseName, req.Args[0], req.Resource}, nil
}

// brokeredWatchCap is the scope a brokered watch moves into, out of the
// broker's unit and its memory limit: the standby watch's own limit, twice.
var brokeredWatchCap = platform.Cap{Max: "512M", Swap: "0", Slice: "app.slice"}

// ownScope moves a brokered call that outlives its broker's memory limit
// into a scope of its own; anywhere else it stays where it runs.
func ownScope(name string, c platform.Cap) {
	if os.Getenv(sandbox.Brokered) == "" || !plat.Capper.Available() {
		return
	}
	if err := plat.Capper.Adopt(os.Getpid(), fmt.Sprintf("beekeeper-%s-%d", name, os.Getpid()), c); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "beekeeper: %s stays in the broker's unit: %v\n", name, err)
	}
}

// stateFromEnv names a configuration file whose state a beekeeper keeps:
// its stateDir and leaseDir, everything else from the configuration it
// reads. A sandboxed session's agents start --config passes it through the
// broker, so the host's configuration stays the broker's and the start's
// state the scratch one; the started agent runs under it too.
const stateFromEnv = "BEEKEEPER_STATE_FROM"

// stateFrom keeps the state of the configuration file path; in a sandboxed
// session's brokered call, only of one the session reads and whose state
// folders it writes.
func (a *app) stateFrom(path string) error {
	if err := a.sandboxPaths([]string{path}, nil); err != nil {
		return err
	}
	c, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("$%s: %w", stateFromEnv, err)
	}
	if err := a.sandboxPaths(nil, []string{c.StateDir, c.LeaseDir}); err != nil {
		return err
	}
	a.cfg.StateDir, a.cfg.LeaseDir = c.StateDir, c.LeaseDir
	return nil
}

// sandboxPaths holds a sandboxed session's brokered call to the agent
// sandbox's lists: through the broker it reads and writes no path its
// sandbox closes to it. Outside a brokered call of a sandboxed session it
// holds nothing.
func (a *app) sandboxPaths(read, write []string) error {
	if os.Getenv(sandbox.Brokered) == "" || os.Getenv(sandbox.Env) == "" {
		return nil
	}
	p := a.sandboxPolicy()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	for _, f := range read {
		if f != "" && !p.Readable(f, cwd) {
			return refused("the agent sandbox does not let this session read %s", f)
		}
	}
	for _, f := range write {
		if f != "" && !p.Writable(f, cwd) {
			return refused("the agent sandbox does not let this session write %s", f)
		}
	}
	return nil
}
