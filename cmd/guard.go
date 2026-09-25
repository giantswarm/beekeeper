package cmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) runCmd() *cobra.Command {
	var maxFlag, swapFlag, waitFlag string
	c := &cobra.Command{
		Use:   "run [--max SIZE] [--wait DURATION] -- command [args...]",
		Short: "Run a build, test or lint command in a build slot under a memory cap",
		Long: `run takes one of the machine's build slots and runs the command in a
transient systemd user scope under memcap.slice with MemoryMax=SIZE,
MemorySwapMax=0 and OOMPolicy=continue. When the command's process tree
reaches the cap the kernel kills its biggest process and nothing outside the
scope notices; run then names the victim in one line starting with
"` + guard.LogPrefix + `" and exits 137 (or the tool's own failure code).
Bound the command's parallelism rather than raising --max.

When every slot is held, or MemAvailable is below the cap, run waits up to
--wait, one line when the wait starts and one when it ends. A wait that runs
out exits 75 and lists the holders: run the command in the background
instead (the PreToolUse hook then sets a 60m wait), never poll.

The slots are memcap's flock files, shared with the memcap wrapper. The
command's arguments reach it verbatim: systemd-run's own ${VAR} expansion is
off. Without a user systemd (containers, CI) the command runs uncapped.

Every capped run leaves a run.start and a run.end event in beekeeper log,
naming the scope, the session and the command, so that a cap kill found
later (snapshot, watch) names them after the run has ended. Logging never
fails or delays the run: an event the log cannot take within a second is
dropped.

Environment: MEMCAP_MAX (12G), MEMCAP_SWAP (0), MEMCAP_WAIT (8m),
MEMCAP_SLOTS and MEMCAP_STATE (the directory holding slots/) override the
configuration; the flags override the environment.`,
		Args: cobra.MinimumNArgs(1),
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return a.loadConfig()
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			o := guard.Options{Max: env("MEMCAP_MAX", "12G"), Swap: env("MEMCAP_SWAP", "0"),
				SlotDir: a.cfg.Memcap.SlotDir, Slots: a.cfg.Memcap.Slots, Stderr: os.Stderr, Record: a.runRecorder()}
			if s := os.Getenv("MEMCAP_STATE"); s != "" {
				o.SlotDir = filepath.Join(s, "slots")
			}
			if s := os.Getenv("MEMCAP_SLOTS"); s != "" {
				n, err := strconv.Atoi(s)
				if err != nil || n < 1 {
					return usageErr("MEMCAP_SLOTS=%q: want a positive number", s)
				}
				o.Slots = n
			}
			f := cmd.Flags()
			if f.Changed("max") {
				o.Max = maxFlag
			}
			if f.Changed("swap") {
				o.Swap = swapFlag
			}
			wait := env("MEMCAP_WAIT", "8m")
			if f.Changed("wait") {
				wait = waitFlag
			}
			var err error
			if o.Wait, err = guard.ParseWait(wait); err != nil {
				return usageErr("%v", err)
			}
			for _, s := range []string{o.Max, o.Swap} {
				if _, err := guard.ParseSize(s); err != nil {
					return usageErr("%v", err)
				}
			}
			if rc := guard.Run(o, args); rc != 0 {
				return &exitError{code: rc}
			}
			return nil
		},
	}
	c.Flags().SetInterspersed(false)
	c.Flags().StringVar(&maxFlag, "max", "", "the command's memory cap, a systemd size (default $MEMCAP_MAX or 12G)")
	c.Flags().StringVar(&swapFlag, "swap", "", "the command's swap cap (default $MEMCAP_SWAP or 0)")
	c.Flags().StringVar(&waitFlag, "wait", "", "how long to wait for a slot and memory (default $MEMCAP_WAIT or 8m)")
	return c
}

// noSession names a process no Claude Code session started.
const noSession = "no session"

// runRecorder appends a run's events to the event log as the calling
// session, or nil when the log cannot be opened: a build runs regardless.
func (a *app) runRecorder() func(verb, detail string) {
	st, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return nil
	}
	by, err := a.caller()
	if err != nil {
		by = state.Party{Name: noSession}
	}
	return func(verb, detail string) {
		_ = st.Log(state.Event{At: time.Now(), By: by, Verb: verb, Detail: detail})
	}
}

func (a *app) hookCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "hook",
		Short: "Claude Code hooks",
		Args:  cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.AddCommand(&cobra.Command{
		Use:   "pretooluse",
		Short: "The Bash tool's PreToolUse hook: builds into a slot, at most two kind labs, the merge gate",
		Long: `pretooluse reads a PreToolUse event on stdin. A build, test, lint or lab
command is rewritten to run through "beekeeper run -- zsh -c '<command>'"
(the absolute path of this binary), the tool timeout raised to 10 minutes;
a background run gets a 60-minute wait instead. A command that would start a
third kind cluster is refused with the running labs and the held leases.
Every devctl pr merge gets "<this binary> gate --" in front of it (a
background one "gate --wait 30m --"), the timeout raised the same way; see
beekeeper lanes. Other devctl commands pass untouched.
Anything else, malformed input included, passes unchanged.

Register it in ~/.claude/settings.json:

  "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command",
    "command": "~/.go/bin/beekeeper hook pretooluse"}]}]`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			defer func() { _ = recover() }() // a broken hook must not block the tool call
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				return nil
			}
			self, _ := os.Executable()
			h := guard.Hook{Self: self, Clusters: kindClusterNames, Leases: a.heldLeases}
			if out := h.Decide(raw); out != nil {
				_, _ = a.out.Write(out)
			}
			return nil
		},
	})
	return c
}

// loadConfig reads the configuration without opening the state.
func (a *app) loadConfig() error {
	path, err := config.Path(a.cfgPath)
	if err != nil {
		return err
	}
	a.cfg, err = config.Load(path)
	return err
}

func kindClusterNames() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cs, _ := machine.KindClusters(ctx)
	names := make([]string, 0, len(cs))
	for _, c := range cs {
		names = append(names, c.Name)
	}
	return names
}

// heldLeases lists the held leases for the third-lab refusal, each holder
// named as `lease list` names it. The configuration is read only when a
// refusal needs it: a broken one must not block a tool call.
func (a *app) heldLeases() []lease.Holder {
	if a.loadConfig() != nil {
		return nil
	}
	hs, _ := lease.Dir(a.cfg.LeaseDir).List()
	var sessions []*claude.Session
	if t, err := proc.Read(); err == nil {
		sessions = claude.Discover(a.cfg, t, time.Now())
	}
	return a.namedHolders(sessions, hs)
}

// namedHolders sets each holder's name to the one `lease list` shows: the
// claim's, the live session's, or the user@host holder.
func (a *app) namedHolders(sessions []*claude.Session, hs []lease.Holder) []lease.Holder {
	out := make([]lease.Holder, len(hs))
	for i, h := range hs {
		h.Name = a.leaseView(sessions, h).Name
		out[i] = h
	}
	return out
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
