package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

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
		argv, err := brokerArgv(cmd, args)
		if err != nil {
			return err
		}
		return a.brokeredAnswer(sandbox.Request{Op: op, Args: argv}, timeout, true)
	}
	return c
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

// brokerFlags is cmd's name and the flags set on it, for a brokered call.
// --as and --config are refused: a brokered call runs as its session,
// under the host's config.
func brokerFlags(cmd *cobra.Command) ([]string, error) {
	argv := []string{cmd.Name()}
	var err error
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if f.Name == "as" || f.Name == "config" {
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
	dir := sandbox.SpoolDir(a.cfg.StateDir)
	if !(sandbox.Capper{Dir: dir}).Available() {
		return refused("no sandbox broker answers in %s: beekeeper-sandbox.service on the host runs this for the sandbox (beekeeper install)", dir)
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
