package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The gate's own exit codes, apart from devctl's 1-9 and the busy machine's 75.
const (
	// ExitGateQueued: the merge keeps its place; run the same command again.
	ExitGateQueued = 76
	// ExitGateRefused: a hold, the budget floor or an unreadable installation.
	ExitGateRefused = 77
	// GatePrefix starts every line the gate prints.
	GatePrefix = "beekeeper gate: "
	// DefaultGateWait is a foreground merge's wait for its turn; the hook
	// gives a background one BackgroundGateWait.
	DefaultGateWait    = 2 * time.Minute
	BackgroundGateWait = 30 * time.Minute
)

func (a *app) gateCmd() *cobra.Command {
	var wait time.Duration
	c := &cobra.Command{
		Use:   "gate [--wait DURATION] -- devctl pr merge <owner/repo> <n> [flags]",
		Short: "The PreToolUse hook's gate on devctl pr merge",
		Long: `gate is what the PreToolUse hook puts in front of every devctl pr merge; a
session never calls it. It refuses the merge (exit 77) when the repository,
its lane, "merges" or "github" is held, when the GitHub budget is under the
floor or unknown, or when the lane's installation cannot be read. Otherwise
the merge joins its lane's queue and runs when it is first, nothing else of
the lane runs, the lane's HelmReleases are Ready and the previous merge's
release has rolled, and fewer than merge.cap devctl processes run. A wait
longer than --wait exits 76 and keeps the merge's place for merge.queueTTL.
devctl then runs once; its document and exit code pass through unchanged.`,
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		PersistentPreRunE: func(*cobra.Command, []string) error {
			if err := a.load(); err != nil {
				return gateRefused("the configuration does not load (%v): fix it, then run the same command again", err)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.gate(cmd.Context(), args, wait)
		},
	}
	c.Flags().SetInterspersed(false)
	c.Flags().DurationVar(&wait, "wait", DefaultGateWait, "how long to wait for the merge's turn")
	return c
}

func gateLine(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, GatePrefix+format+"\n", args...)
}

func gateRefused(format string, args ...any) error {
	gateLine("refused, "+format, args...)
	return &exitError{code: ExitGateRefused}
}

// gateRun is one gated merge.
type gateRun struct {
	*app
	ctx     context.Context
	argv    []string
	repo    string
	pr      int
	lane    config.Lane
	me      state.Party
	pid     int
	lastWhy string
	seeded  bool // the merge's place was queued on the session's behalf
}

func (a *app) gate(ctx context.Context, argv []string, wait time.Duration) error {
	repo, pr, ok := merge.ParseArgs(argv)
	if !ok {
		return exitCode(runChild(argv, os.Stdout))
	}
	me, err := a.caller()
	if err != nil {
		me = state.Party{Name: fmt.Sprintf("pid %d", os.Getppid())}
	}
	g := &gateRun{app: a, ctx: ctx, argv: argv, repo: repo, pr: pr, lane: a.cfg.LaneOf(repo), me: me, pid: os.Getpid()}
	deadline := time.Now().Add(wait)
	for {
		a.now = time.Now()
		why, err := g.step()
		if err != nil || why == "" {
			return err
		}
		if !a.now.Before(deadline) {
			kept := a.cfg.Merge.QueueTTL.Duration
			if g.seeded {
				kept = a.cfg.Merge.SeedTTL.Duration
			}
			gateLine("queued, %s; your place is kept for %s: run the same command again with run_in_background (the wait is then %s), do not poll",
				why, kept, BackgroundGateWait)
			return &exitError{code: ExitGateQueued}
		}
		if why != g.lastWhy {
			gateLine("waiting (up to %s): %s", deadline.Sub(a.now).Round(time.Second), why)
			g.lastWhy = why
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(deadline.Sub(a.now), 5*time.Second)):
		}
	}
}

// step joins or refreshes the merge's queue entry and starts devctl when it
// is the merge's turn. It returns what the merge waits for, or "" and the
// run's outcome once devctl ran or the merge was refused.
func (g *gateRun) step() (string, error) {
	g.closeToolWindow()
	var q merge.Lane
	var hold state.Hold
	var held bool
	var dup *state.Merge
	err := g.store.Update(func(st *state.State) ([]state.Event, error) {
		merge.Prune(st, g.now, g.cfg.Merge.QueueTTL.Duration, g.cfg.Merge.SeedTTL.Duration, proc.Alive)
		if hold, held = merge.Blocking(st, g.now, g.repo, g.lane.Name); held {
			g.drop(st)
			return nil, nil
		}
		for i, m := range st.Merges {
			if m.Repo == g.repo && m.PR == g.pr && m.Phase == state.Running && m.PID != g.pid {
				dup = &st.Merges[i]
				return nil, nil
			}
		}
		var ev []state.Event
		if i := g.mine(st, state.Waiting); i >= 0 {
			st.Merges[i].PID, st.Merges[i].By, st.Merges[i].Seen = g.pid, g.me, g.now.UTC()
			g.seeded = st.Merges[i].Seeded
		} else {
			st.Merges = append(st.Merges, state.Merge{Repo: g.repo, PR: g.pr, Lane: g.lane.Name, By: g.me, PID: g.pid,
				Phase: state.Waiting, Joined: g.now.UTC(), Seen: g.now.UTC()})
			ev = append(ev, event(g.me, "merge.queued", "%s#%d in lane %s", g.repo, g.pr, g.lane.Name))
		}
		q = merge.Queue(st, g.lane.Name)
		return ev, nil
	})
	switch {
	case err != nil:
		return "", gateRefused("the state does not load (%v): fix it, then run the same command again", err)
	case held:
		return "", g.refuse("%s is held (%s) by %q until %s: %s; merge after the hold lifts (beekeeper hold), do not poll",
			g.repo, hold.Target, hold.By.Name, untilText(g.app, hold), hold.Reason)
	case dup != nil:
		return "", g.refuse("%s#%d is already merging in %q (pid %d): let that run finish", g.repo, g.pr, dup.By.Name, dup.PID)
	}
	if pos := q.Position(g.repo, g.pr); pos > 1 {
		ahead := q.Waiting[0]
		if q.Running != nil {
			ahead = *q.Running
		}
		phase := ahead.Phase
		if phase == state.Waiting && !proc.Alive(ahead.PID) {
			phase = "queued, its merge has not arrived"
		}
		return fmt.Sprintf("position %d in lane %s behind %s (%q, %s)", pos, g.lane.Name, ahead.Key(), ahead.By.Name, phase), nil
	}
	if q.Running != nil {
		return fmt.Sprintf("next in lane %s behind the running %s (%q, since %s)", g.lane.Name, q.Running.Key(), q.Running.By.Name,
			clock(g.now, q.Running.Started)), nil
	}
	hrs, why, err := g.laneReady(q.Settling)
	if err != nil || why != "" {
		return why, err
	}
	b, err := g.gateBudget()
	if err != nil {
		return "", g.refuse("the GitHub budget is unknown (%v): fix that (gh auth status), then run the same command again", err)
	}
	if b.Remaining < g.cfg.GitHub.Floor {
		return "", g.refuse("the GitHub budget %d is under the floor %d: merge after the reset at %s, do not poll",
			b.Remaining, g.cfg.GitHub.Floor, clock(g.now, b.Reset))
	}
	return g.start(q.Settling, hrs)
}

// mine is the index of this merge's entry in that phase, -1 when none.
func (g *gateRun) mine(st *state.State, phase string) int {
	return slices.IndexFunc(st.Merges, func(m state.Merge) bool {
		return m.Repo == g.repo && m.PR == g.pr && m.Phase == phase && (phase == state.Waiting || m.PID == g.pid)
	})
}

// drop removes the merge's waiting entry; a seeded place stays.
func (g *gateRun) drop(st *state.State) {
	if i := g.mine(st, state.Waiting); i >= 0 && !st.Merges[i].Seeded {
		st.Merges = slices.Delete(st.Merges, i, i+1)
	}
}

// refuse removes the merge from its queue, logs the refusal and refuses it.
func (g *gateRun) refuse(format string, args ...any) error {
	why := fmt.Sprintf(format, args...)
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		g.drop(st)
		return []state.Event{event(g.me, "merge.refused", "%s#%d: %s", g.repo, g.pr, why)}, nil
	})
	return gateRefused("%s", why)
}

// laneReady reads the lane's installation: why is what the lane waits for,
// "" when it is free. An installation that cannot be read refuses.
func (g *gateRun) laneReady(settling *state.Merge) ([]merge.HelmRelease, string, error) {
	if g.lane.Installation == "" {
		return nil, "", nil
	}
	hrs, err := readHelmReleases(g.ctx, g.lane)
	if err != nil {
		return nil, "", g.refuse("lane %s cannot read the HelmReleases of %s (%v): log in (tsh kube login %s), then run the same command again",
			g.lane.Name, g.lane.Installation, err, g.lane.Installation)
	}
	ready, why := merge.Ready(g.lane, hrs, settling, g.now, g.cfg.Merge.Settle.Duration)
	if ready {
		return hrs, "", nil
	}
	if settling != nil && g.now.Sub(settling.Finished) > g.cfg.Merge.SettleTimeout.Duration {
		return nil, "", g.refuse("lane %s has waited %s since %s: %s; fix the installation or clear the lane (beekeeper lanes clear %s), then run the same command again",
			g.lane.Name, g.cfg.Merge.SettleTimeout.Duration, settling.Key(), why, g.lane.Name)
	}
	return nil, fmt.Sprintf("next in lane %s, waiting for %s: %s", g.lane.Name, g.lane.Installation, why), nil
}

// start makes the merge the lane's running one, when it still is first and
// the machine runs fewer than merge.cap devctl processes, and runs devctl.
func (g *gateRun) start(settling *state.Merge, hrs []merge.HelmRelease) (string, error) {
	toolFrom := ""
	if strings.EqualFold(g.repo, merge.ToolRepo) {
		toolFrom = toolVersion(g.ctx)
	}
	why := ""
	err := g.store.Update(func(st *state.State) ([]state.Event, error) {
		q := merge.Queue(st, g.lane.Name)
		i := g.mine(st, state.Waiting)
		switch {
		case i < 0 || q.Position(g.repo, g.pr) != 1 || q.Running != nil:
			why = "the lane moved on"
			return nil, nil
		case q.Settling != nil && (settling == nil || q.Settling.Key() != settling.Key()):
			why = "another merge of the lane just settled"
			return nil, nil
		}
		if n := devctlRuns(st); n >= g.cfg.Merge.Cap {
			why = fmt.Sprintf("%d devctl processes run machine-wide (cap %d), not a lane problem: %s#%d is next in lane %s and starts when one ends",
				n, g.cfg.Merge.Cap, g.repo, g.pr, g.lane.Name)
			return nil, nil
		}
		st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool { return m.Lane == g.lane.Name && m.Phase == state.Settling })
		i = g.mine(st, state.Waiting)
		m := &st.Merges[i]
		m.Phase, m.Started, m.Roll, m.Seeded = state.Running, g.now.UTC(), merge.RollSet(hrs, g.repo), false
		ev := []state.Event{event(g.me, "merging", "%s#%d in lane %s", g.repo, g.pr, g.lane.Name)}
		if strings.EqualFold(g.repo, merge.ToolRepo) {
			st.Holds = slices.DeleteFunc(st.Holds, func(h state.Hold) bool { return h.Target == merge.AllMerges })
			h := state.Hold{Target: merge.AllMerges, Except: merge.ToolRepo, By: g.me, At: g.now.UTC(), Tool: merge.Tool, ToolFrom: toolFrom,
				Reason: fmt.Sprintf("the devctl release window: %s#%d merges and every devctl run refuses until updated; it lifts once `devctl version` reports the release", g.repo, g.pr)}
			st.Holds = append(st.Holds, h)
			ev = append(ev, event(g.me, "hold.set", "%s except %s until devctl is updated: %s", h.Target, h.Except, h.Reason))
		}
		return ev, nil
	})
	if err != nil {
		return "", g.refuse("the state does not load (%v): fix it, then run the same command again", err)
	}
	if why != "" {
		return why, nil
	}
	return "", g.runMerge()
}

// runMerge runs devctl once, its document and exit code unchanged, and
// records the outcome: a merge settles its lane, anything else leaves it.
func (g *gateRun) runMerge() error {
	var doc bytes.Buffer
	rc := runChild(g.argv, io.MultiWriter(os.Stdout, &doc))
	out, ok := merge.ParseDocument(doc.Bytes())
	if !ok {
		out = merge.Outcome{Merged: rc == 0 || rc == 9}
	}
	now := time.Now().UTC()
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		i := g.mine(st, state.Running)
		if i >= 0 && out.Merged && !out.NoRelease {
			m := &st.Merges[i]
			m.Phase, m.Finished, m.Exit, m.Release = state.Settling, now, rc, out.Release
		} else if i >= 0 {
			st.Merges = slices.Delete(st.Merges, i, i+1)
		}
		if strings.EqualFold(g.repo, merge.ToolRepo) {
			for j, h := range st.Holds {
				if h.Tool != "" && out.Merged {
					st.Holds[j].ToolRelease = out.Release
				}
			}
			if !out.Merged {
				st.Holds = slices.DeleteFunc(st.Holds, func(h state.Hold) bool { return h.Tool != "" })
			}
		}
		release := out.Release
		switch {
		case out.NoRelease:
			release = "none warranted"
		case release == "":
			release = "unknown"
		}
		if !out.Merged {
			return []state.Event{event(g.me, "merge.failed", "%s#%d exit %d, nothing merged", g.repo, g.pr, rc)}, nil
		}
		return []state.Event{event(g.me, "merged", "%s#%d exit %d, release %s", g.repo, g.pr, rc, release)}, nil
	})
	return exitCode(rc)
}

// closeToolWindow lifts a tool-release window once no merge of the tool's
// repository runs and the tool reports another version than at its opening.
func (g *gateRun) closeToolWindow() {
	st, err := g.store.Read()
	if err != nil || !slices.ContainsFunc(st.Holds, func(h state.Hold) bool { return h.Tool != "" }) {
		return
	}
	if slices.ContainsFunc(st.Merges, func(m state.Merge) bool {
		return strings.EqualFold(m.Repo, merge.ToolRepo) && m.Phase == state.Running && proc.Alive(m.PID)
	}) {
		return
	}
	v := toolVersion(g.ctx)
	if v == "" {
		return
	}
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		var ev []state.Event
		st.Holds = slices.DeleteFunc(st.Holds, func(h state.Hold) bool {
			if h.Tool == "" || h.ToolFrom == v {
				return false
			}
			ev = append(ev, event(g.me, "hold.lift", "%s: devctl now reports %s (the window opened on %s)", h.Target, v, h.ToolFrom))
			return true
		})
		return ev, nil
	})
}

// gateBudget is the last budget reading when it is younger than
// merge.budgetFresh, else a fresh one (a conditional request, free on 304).
func (g *gateRun) gateBudget() (state.Budget, error) {
	st, err := g.store.Read()
	if err != nil {
		return state.Budget{}, err
	}
	if b := st.Budget; b != nil && g.now.Sub(b.At) <= g.cfg.Merge.BudgetFresh.Duration {
		return *b, nil
	}
	ctx, cancel := context.WithTimeout(g.ctx, 30*time.Second)
	defer cancel()
	b, err := g.probeBudget(ctx)
	return state.Budget{Remaining: b.Remaining, Limit: b.Limit, Reset: b.Reset}, err
}

// devctlRuns counts the machine's devctl processes, a running merge's own
// once even before its devctl started.
func devctlRuns(st *state.State) int {
	running := map[int]bool{}
	for _, m := range st.Merges {
		if m.Phase == state.Running {
			running[m.PID] = true
		}
	}
	n := len(running)
	t, err := proc.Read()
	if err != nil {
		return n
	}
	for _, p := range t.ByPID {
		if p.Comm == merge.Tool && !running[p.PPID] {
			n++
		}
	}
	return n
}

// toolVersion is what `devctl version` reports, "" when it does not run.
func toolVersion(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, merge.Tool, "version").Output() //nolint:gosec // the merge tool, a constant
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "Version:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// kubeContext is the lane's context, else the kubeconfig context named
// after its installation or ending in -<installation>.
func kubeContext(ctx context.Context, lane config.Lane) (string, error) {
	if lane.Context != "" {
		return lane.Context, nil
	}
	out, err := exec.CommandContext(ctx, "kubectl", "config", "get-contexts", "-o", "name").Output()
	if err != nil {
		return "", fmt.Errorf("kubectl config get-contexts: %w", err)
	}
	var found []string
	for name := range strings.FieldsSeq(string(out)) {
		if name == lane.Installation || strings.HasSuffix(name, "-"+lane.Installation) {
			found = append(found, name)
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("%d kubeconfig contexts match %s: set lanes[].context", len(found), lane.Installation)
	}
	return found[0], nil
}

// readHelmReleases reads the lane installation's HelmReleases with kubectl.
func readHelmReleases(ctx context.Context, lane config.Lane) ([]merge.HelmRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	kctx, err := kubeContext(ctx, lane)
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	c := exec.CommandContext(ctx, "kubectl", "--context", kctx, "get", "helmreleases.helm.toolkit.fluxcd.io", "-A", "-o", "json") //nolint:gosec // the configured context
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
			msg = msg[i+1:]
		}
		return nil, fmt.Errorf("context %s: %v: %s", kctx, err, msg)
	}
	return merge.ParseHelmReleases(out)
}

// runChild runs the command with this process's stdin and stderr, its
// stdout into out, forwarding signals, and returns its exit code.
func runChild(argv []string, out io.Writer) int {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		gateLine("%v", err)
		return guard.ExitNotFound
	}
	c := exec.Command(path, argv[1:]...) //nolint:gosec // running the caller's command is the purpose
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, out, os.Stderr
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	if err := c.Start(); err != nil {
		gateLine("%v", err)
		return guard.ExitNotFound
	}
	go func() {
		for s := range sig {
			_ = c.Process.Signal(s)
		}
	}()
	_ = c.Wait()
	if ws, ok := c.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return c.ProcessState.ExitCode()
}

func exitCode(rc int) error {
	if rc == 0 {
		return nil
	}
	return &exitError{code: rc}
}
