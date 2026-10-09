package cmd

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// The gate's own exit codes, apart from devctl's 1-9 and the busy machine's 75.
const (
	// ExitGateQueued: the merge waits on for its turn, or for the GitHub
	// budget's reset, in a run of its own, which wakes its owner with the
	// outcome.
	ExitGateQueued = 76
	// ExitGateRefused: a hold, a GitHub budget unknown or an unreadable
	// installation; nothing is queued.
	ExitGateRefused = 77
	// ExitGateDuplicate: the pull request's merge already runs, devctl's
	// "not applicable".
	ExitGateDuplicate = 3
	// GatePrefix starts every line the gate prints.
	GatePrefix = "beekeeper gate: "
	// DefaultGateWait is a foreground merge's wait for its turn before it
	// waits on in a run of its own; the hook gives a background one
	// BackgroundGateWait.
	DefaultGateWait    = 2 * time.Minute
	BackgroundGateWait = 30 * time.Minute
	// gateCmdName is the gate's command, which a queued run and a brokered
	// gate run, and which re-executes a replaced binary by itself.
	gateCmdName = "gate"
	// gateDeadlineEnv carries a waiting call's deadline across the re-exec
	// of a replaced binary.
	gateDeadlineEnv = "BEEKEEPER_GATE_DEADLINE"
	// gateRunningEnv carries a running merge's devctl across the re-exec of
	// a replaced binary: its merge-child's pid and how far its stderr was
	// copied, "<pid>:<offset>".
	gateRunningEnv = "BEEKEEPER_GATE_RUNNING"
	// gateFromEnv names the gate a queued run took the merge's place from.
	gateFromEnv = "BEEKEEPER_GATE_FROM"
)

func (a *app) gateCmd() *cobra.Command {
	var wait time.Duration
	var queued bool
	c := &cobra.Command{
		Use:   gateCmdName + " [--wait DURATION] -- devctl pr merge|release promote|pr wait|release wait|rollout wait <args>",
		Short: "The PreToolUse hook's gate on devctl's blocking commands",
		Long: `gate is what the PreToolUse hook puts in front of every devctl pr merge and
every devctl release promote of one repository; a session never calls it. A
promotion queues and runs in its repository's lane like a merge, its stable
release settling the lane. Only a hold refuses (exit 77): the repository,
its lane, "merges" or "github" held (a cluster upgrade on the lane's
installation holds it too); so do a GitHub budget unknown and a lane
installation that cannot be read. A rebase merge (--rebase) GitHub reports
not rebaseable or conflicting with its base is refused (77) before it takes
a lane place, the way out a replacement PR; GitHub not answering whether it
rebases refuses too. Under the budget floor the merge is
queued for the reset (exit 76, as below): a run of its own waits for the
budget and merges as below; a merge marked urgent (lanes urgent)
runs under the floor instead, once per reset window. Otherwise the merge
joins its lane's queue (a merge registered with lanes settle heads it) and
runs when no merge before it holds its place (one in the gate or within
merge.queueTTL of its last run; for a seeded place also the seeds before
it), nothing else of the lane runs, the lane's HelmReleases are Ready and
the previous merge's release has rolled, and fewer than merge.cap devctl
processes run. A wait longer than --wait hands the merge to a run of its
own outside the caller (exit 76): the same gate under --queued keeps the
merge's place, waits up to merge.seedTTL, runs devctl when its turn comes
and wakes the owner with the outcome; a second merge of the pull request
is refused (exit 3) while it waits. A place whose pull request merged or
closed outside the gate leaves the queue at the next check (watch, and a
merge waiting behind it). A call whose binary is replaced (beekeeper
self-update, install) re-executes the new one at its next step: the same
process, arguments and stdio; waiting, the same place and deadline;
running, the same devctl, followed on from where its stderr was copied, so
the installed release records the outcome. devctl runs once, in a session
of its own, so it merges on when the caller's session ends (only SIGINT
reaches it); its document and exit code pass through unchanged. A call
whose release is older than the one that wrote the state and that cannot
re-execute is refused (77) before it touches the lane: the installed
beekeeper gates the same command again; one whose devctl already ran leaves
the run's document and exit code for the watch, which records the outcome
at its next poll. A merge into a base branch no Auto-release
run tags (the Auto-release workflow on the branch, read once per merge,
names no push to it; its tags are cut by hand) runs devctl with
--no-release-wait: its lane frees the moment devctl reports it merged and
no release is awaited; GitHub not answering for the base refuses (77). A
SIGTERM aimed at the gate, its caller still there two seconds later,
stops devctl too, and so does a TaskStop of the background task the gate
runs (the PreToolUse hook ends its devctl first). A merge call GitHub
answers with a 5xx is sent again while the pull request is open, up to
three times (10s, 30s, 1m apart); the caller reads the last document. A run
with nothing merged leaves the lane with its run. A run without its
document or ended by a signal is judged by GitHub: merged, its release is
unconfirmed. A second merge of a pull request whose merge runs is refused
(exit 3) with that run's start, owner and last line.

/home/teemow/.go/bin/beekeeper gate -- devctl pr wait, release wait and rollout wait run the same way outside
their caller, without a queue; one whose identical command already runs on
the machine follows that run (follow-run: its stderr, document and exit
code) instead of polling GitHub a second time. Whichever command it is, its outcome reaches
the session that started it: the caller sees the output and exit code as
ever while it listens, and when it no longer does (a headless turn that
ended, a caller killed, its CLI gone, a queued merge) the run wakes its
owner, a registered agent, with one line: "<command> exit N: <reason>
(output in <file>)", logged as devctl.unheard.`,
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		PersistentPreRunE: func(_ *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				if _, _, ok := parseGated(args); ok {
					return gateRefused("the configuration does not load (%v): fix it, then run the same command again", err)
				}
				gateLine("the configuration does not load (%v): this runs unowned, its outcome reaches nobody if your turn ends first", err)
				a.store = nil
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.gate(cmd.Context(), args, wait, queued)
		},
	}
	c.Flags().SetInterspersed(false)
	c.Flags().DurationVar(&wait, "wait", DefaultGateWait, "how long to wait for the merge's turn")
	c.Flags().BoolVar(&queued, "queued", false, "wait as a queued merge's own run, which refuses at the end of --wait")
	_ = c.Flags().MarkHidden("queued")
	return c
}

func gateLine(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, GatePrefix+format+"\n", args...)
}

func gateRefused(format string, args ...any) error {
	return gateRefusedWith(ExitGateRefused, format, args...)
}

func gateRefusedWith(code int, format string, args ...any) error {
	gateLine("refused, "+format, args...)
	return &exitError{code: code}
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
	cli     int // the caller's CLI (callerCLI)
	lastWhy string
	seeded  bool // the merge's place was queued on the session's behalf
	queued  bool // the merge's own run, which waits on after exit 76
	from    int  // the gate a queued run took the merge's place from
	// placed says the merge had its place in the lane: a step that finds
	// none, its place was taken out (lanes drop).
	placed bool
	// candidate is the release candidate a promotion is for; read says this
	// call read it (a fresh promote), else it is its place's.
	candidate     string
	candidateRead bool
	// central says the merge's lane queues in the central instance: joined
	// once the merge has its place there, centralWhy what it waits for
	// there as of centralAsked.
	central      bool
	joined       bool
	centralWhy   string
	centralAsked time.Time
	// release is how the pull request's base branch releases, read once
	// before devctl's turn.
	release *github.BaseRelease
	// bin is the executable this call runs, re-executed once replaced.
	bin binary
	// output is the file this call's stdout writes to (gateOutput), the
	// background task a TaskStop names.
	output string
}

func (a *app) gate(ctx context.Context, argv []string, wait time.Duration, queued bool) error {
	if inSandbox() {
		return a.gateBrokered(argv, wait, queued)
	}
	argv, detached := merge.StripDetach(argv)
	if len(detached) > 0 {
		gateLine("dropped %s: the gate runs the merge outside your session already, its outcome reaches you as for the blocking form",
			strings.Join(detached, " "))
	}
	repo, pr, ok := parseGated(argv)
	if !ok {
		if a.store != nil && merge.ParseOwned(argv) {
			return a.ownedRun(argv)
		}
		return exitCode(runChild(argv, os.Stdout))
	}
	me, err := a.caller()
	if err != nil {
		me = state.Party{Name: fmt.Sprintf("pid %d", os.Getppid())}
	}
	g := &gateRun{app: a, ctx: ctx, argv: argv, repo: repo, pr: pr, lane: a.cfg.LaneOf(repo), me: me, pid: os.Getpid(), cli: callerCLI(), queued: queued,
		bin: runningBinary(), output: gateOutput()}
	g.central = pr != 0 && a.cfg.CentralLane(g.lane)
	if v, ok := os.LookupEnv(gateRunningEnv); ok {
		_ = os.Unsetenv(gateRunningEnv)
		return g.resumeMerge(v)
	}
	// A queued run takes over the place its gate holds: none means dropped.
	g.placed = queued
	if pr == 0 && !queued {
		if g.candidate, err = promoteCandidate(ctx, repo); err != nil {
			return gateRefused("the release candidate of %s is unknown (%v): nothing promoted; run the same command again", repo, err)
		}
		g.candidateRead = true
	}
	if v, ok := os.LookupEnv(gateFromEnv); ok {
		_ = os.Unsetenv(gateFromEnv)
		g.from, _ = strconv.Atoi(v)
	}
	// A fresh rebase merge, not one that has its place already (its queued
	// run, a re-executed wait), is judged before it takes a lane place.
	if _, waiting := os.LookupEnv(gateDeadlineEnv); !waiting && !queued && merge.Rebase(argv) {
		if err := g.checkRebase(); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(wait)
	if v, ok := os.LookupEnv(gateDeadlineEnv); ok {
		_ = os.Unsetenv(gateDeadlineEnv) // devctl must not inherit it
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			deadline = t
			gateLine("continuing under %s %s: %s keeps its place in lane %s", project.Name, project.Version(), g.key(), g.lane.Name)
		}
	}
	for {
		a.now = time.Now()
		// Before the step saves: the new release may have written the
		// state, which refuses this binary's save.
		if g.bin.Replaced() {
			g.reexec("while the merge waited", gateDeadlineEnv+"="+deadline.Format(time.RFC3339Nano))
		}
		why, err := g.step()
		var stale *state.StaleWriterError
		if errors.As(err, &stale) {
			// The step saved nothing: a replaced binary carries on as the
			// installed one, with the same place.
			if g.bin.Replaced() {
				g.reexec("while the merge waited", gateDeadlineEnv+"="+deadline.Format(time.RFC3339Nano))
			}
			return g.staleRefused(stale)
		}
		if err != nil || why == "" {
			return err
		}
		if !a.now.Before(deadline) {
			if g.queued {
				return g.refuse("waited %s for its turn, %s; its place is dropped: run the same command again once the lane moves", wait, why)
			}
			return g.enqueue(why)
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

// reexec replaces this process with the binary installed at its path, with
// env for the new one, and says so; it returns only when the new binary
// does not start, and the call carries on under this one.
func (g *gateRun) reexec(when string, env ...string) {
	gateLine("%s was replaced %s: re-executing it", g.bin.Path(), when)
	err := g.bin.Exec(env...)
	gateLine("the new binary does not start (%v): carrying on under %s", err, project.Version())
}

// staleRefused refuses the call whose save the newer release that wrote the
// state refused and that could not re-execute: nothing ran, the lane is
// untouched, and the installed beekeeper gates the same command again.
func (g *gateRun) staleRefused(stale *state.StaleWriterError) error {
	why := fmt.Sprintf("%s: this call runs beekeeper %s, older than the %s that wrote the state; nothing ran, the lane is untouched: run the same command again, the installed %s gates it",
		g.key(), stale.Version, stale.Newer, project.Name)
	_ = g.store.Log(event(g.me, "merge.refused", "%s", why))
	return gateRefused("%s", why)
}

// resumeMerge carries a running merge on after the re-exec of a replaced
// binary: running names its merge-child and how far its stderr was copied
// (gateRunningEnv). The merge keeps its place, its devctl runs on, and this
// release follows and records it; a merge the state no longer runs is
// followed to its end all the same, its outcome the caller's.
func (g *gateRun) resumeMerge(running string) error {
	defer outliveCaller()()
	pidText, offsetText, _ := strings.Cut(running, ":")
	pid, err := strconv.Atoi(pidText)
	offset, oerr := strconv.ParseInt(offsetText, 10, 64)
	if err != nil || oerr != nil || pid <= 0 {
		return gateRefused("%s: the re-executed call names no running devctl (%s=%q): check the lane (beekeeper lanes); the merge, if it ran, is recorded by the watch", g.key(), gateRunningEnv, running)
	}
	if st, err := g.store.Read(); err == nil {
		if i := g.mine(st, state.Running); i >= 0 && st.Merges[i].HandCut != "" {
			g.release = &github.BaseRelease{Base: st.Merges[i].HandCut}
		}
	}
	gateLine("continuing under %s %s: following %s's devctl (pid %d) on", project.Name, project.Version(), g.key(), pid)
	base, err := g.mergeFiles()
	if err != nil {
		gateLine("%v", err)
		return g.record(base, childRun{rc: guard.ExitNotFound})
	}
	return g.follow(base, pid, offset)
}

// enqueue hands the merge's wait to a run of its own outside the caller
// (launchChild), the same gate under --queued: it keeps the merge's place, merges
// when its turn comes and wakes the owner with the outcome (tellOwner), as
// this gate leaves without its heard marker. It returns exit 76, for a
// merge that waited its turn and for one under the budget floor alike.
func (g *gateRun) enqueue(why string) error {
	queued := &exitError{code: ExitGateQueued}
	self, err := selfExe()
	if err == nil {
		argv := []string{self}
		if c := g.explicitConfig(); c != "" {
			argv = append(argv, "--config", c)
		}
		argv = append(append(argv, gateCmdName, "--queued", "--wait", g.cfg.Merge.SeedTTL.String(), "--"), g.argv...)
		_ = os.Setenv(gateFromEnv, strconv.Itoa(g.pid))
		var pid int
		if pid, err = launchChild(childSpec{Argv: argv, Command: g.argv, Owner: g.me, Config: g.explicitConfig()}, ownedBase(g.store.Dir(), g.argv, g.pid)); err == nil {
			to := "its outcome is logged (beekeeper log --verb merged)"
			if g.me.Session != "" {
				to = fmt.Sprintf("its outcome wakes %q", g.me.Name)
			}
			gateLine("queued, %s; the merge waits on in a run of its own (pid %d) for up to %s and runs when its turn comes; %s: do not run it again, do not poll",
				why, pid, g.cfg.Merge.SeedTTL.Duration, to)
			_ = g.store.Log(event(g.me, "merge.queued", "%s waits on in lane %s in a run of its own (pid %d): %s", g.key(), g.lane.Name, pid, why))
			return queued
		}
	}
	kept := g.cfg.Merge.QueueTTL.Duration
	if g.seeded {
		kept = g.cfg.Merge.SeedTTL.Duration
	}
	gateLine("queued, %s; it cannot wait on in a run of its own (%v): your place is kept for %s, run the same command again with run_in_background, do not poll",
		why, err, kept)
	return queued
}

// step joins or refreshes the merge's queue entry and starts devctl when it
// is the merge's turn. It returns what the merge waits for, or "" and the
// run's outcome once devctl ran or the merge was refused.
func (g *gateRun) step() (string, error) {
	g.closeToolWindow(g.ctx, g.me)
	if why := g.checkOutside(); why != "" {
		return why, nil
	}
	var q merge.Lane
	var hold state.Hold
	var held bool
	var dup *state.Merge
	dropped := false
	err := g.store.Update(func(st *state.State) ([]state.Event, error) {
		merge.Prune(st, g.now, g.cfg.Merge.QueueTTL.Duration, g.cfg.Merge.SeedTTL.Duration, proc.Alive)
		if hold, held = merge.Blocking(st, g.now, g.repo, g.pr, g.lane); held {
			g.drop(st)
			return nil, nil
		}
		for i, m := range st.Merges {
			if m.Repo == g.repo && m.PR == g.pr && m.PID != g.pid && (m.Phase == state.Running ||
				m.Phase == state.Waiting && m.PID != g.from && proc.Alive(m.PID)) {
				dup = &st.Merges[i]
				return nil, nil
			}
		}
		var ev []state.Event
		if i := g.mine(st, state.Waiting); i >= 0 {
			m := &st.Merges[i]
			m.PID, m.By, m.Seen, m.Output = g.pid, g.me, g.now.UTC(), g.output
			g.seeded = m.Seeded
			if g.candidateRead {
				m.Candidate = g.candidate
			} else {
				g.candidate = m.Candidate
			}
		} else if g.placed {
			dropped = true
			return nil, nil
		} else {
			st.Merges = append(st.Merges, state.Merge{Repo: g.repo, PR: g.pr, Lane: g.lane.Name, By: g.me, PID: g.pid, Output: g.output,
				Phase: state.Waiting, Joined: g.now.UTC(), Seen: g.now.UTC(), Candidate: g.candidate})
			detail := fmt.Sprintf("%s in lane %s", g.key(), g.lane.Name)
			if g.pr == 0 {
				detail += ", for " + candidateText(g.candidate)
			}
			ev = append(ev, event(g.me, "merge.queued", "%s", detail))
		}
		g.placed = true
		q = merge.Queue(st, g.lane.Name)
		return ev, nil
	})
	var stale *state.StaleWriterError
	switch {
	case errors.As(err, &stale):
		return "", stale // nothing ran: the gate re-executes a replaced binary, else refuses
	case err != nil:
		return "", gateRefused("the state does not load (%v): fix it, then run the same command again", err)
	case dropped:
		return "", g.refuse("%s was taken out of lane %s (beekeeper lanes drop): nothing ran; run it again only if it is still wanted", g.key(), g.lane.Name)
	case held:
		return "", g.refuse("%s is held (%s) by %q until %s: %s; merge after the hold lifts (beekeeper hold), do not poll",
			g.repo, holdTarget(hold), hold.By.Name, untilText(g.app, hold), hold.Reason)
	case dup != nil && dup.Phase == state.Waiting:
		why := fmt.Sprintf("%s is already queued in lane %s: since %s for %q (pid %d); its outcome reaches %q, do not merge again",
			g.key(), g.lane.Name, clock(g.now, dup.Joined), dup.By.Name, dup.PID, dup.By.Name)
		_ = g.store.Log(event(g.me, "merge.refused", "%s", why))
		return "", gateRefusedWith(ExitGateDuplicate, "%s", why)
	case dup != nil:
		last := lastLine(mergeBase(g.store.Dir(), g.repo, g.pr) + ".log")
		if last == "" {
			last = "none yet"
		}
		return "", g.refuseWith(ExitGateDuplicate, "%s is already merging: started %s by %q (pid %d), last line: %s; its outcome reaches %q, do not merge again",
			g.key(), clock(g.now, dup.Started), dup.By.Name, dup.PID, last, dup.By.Name)
	}
	if g.central {
		if why, err := g.centralTurn(); err != nil || why != "" {
			return why, err
		}
	}
	if ahead, ok := q.Ahead(g.repo, g.pr, g.present); ok {
		if q.Running != nil {
			ahead = *q.Running
		}
		phase := ahead.Phase
		switch {
		case phase != state.Waiting || proc.Alive(ahead.PID):
		case ahead.Outside:
			phase = fmt.Sprintf("settled outside the gate, GitHub reported it not merged at %s", clock(g.now, ahead.Checked))
		case ahead.PID != 0:
			phase = fmt.Sprintf("queued, its merge left the gate at %s, its place is kept", clock(g.now, ahead.Seen))
		default:
			phase = "queued, its merge has not arrived"
		}
		return fmt.Sprintf("position %d in lane %s behind %s (%q, %s)", q.Position(g.repo, g.pr), g.lane.Name, ahead.Key(), ahead.By.Name, phase), nil
	}
	if q.Running != nil {
		return fmt.Sprintf("next in lane %s behind the running %s (%q, since %s)", g.lane.Name, q.Running.Key(), q.Running.By.Name,
			clock(g.now, q.Running.Started)), nil
	}
	hrs, why, err := g.laneReady(q)
	if err != nil || why != "" {
		return why, err
	}
	b, err := g.gateBudget()
	if err != nil {
		return "", g.refuse("the GitHub budget is unknown (%v): fix that (gh auth status), then run the same command again", err)
	}
	urgent := false
	if b.Remaining < g.cfg.GitHub.Floor {
		if urgent, why, err = g.urgentTurn(b); err != nil {
			return "", g.refuse("the state does not load (%v): fix it, then run the same command again", err)
		}
		if !urgent {
			why = cmp.Or(why, fmt.Sprintf("the GitHub budget %d is under the floor %d until the reset at %s", b.Remaining, g.cfg.GitHub.Floor, clock(g.now, b.Reset)))
			if g.queued {
				return why, nil
			}
			return "", g.enqueue(why)
		}
	}
	if err := g.readRelease(); err != nil {
		return "", err
	}
	why, err = g.start(q.SettlingKeys(), hrs)
	if why == "" {
		g.settleUrgent(urgent, b)
	}
	return why, err
}

// checkCandidate refuses a promotion whose turn came, its place running,
// when the newest release candidate is not the one it was queued for: it
// would promote another worker's candidate. Its place leaves the lane and
// its owner decides again.
func (g *gateRun) checkCandidate() error {
	now, err := promoteCandidate(g.ctx, g.repo)
	if err == nil && now == g.candidate {
		return nil
	}
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		if i := g.mine(st, state.Running); i >= 0 {
			st.Merges = slices.Delete(st.Merges, i, i+1)
		}
		return nil, nil
	})
	if err != nil {
		return g.refuse("its turn came, and the release candidate of %s is unknown (%v): nothing promoted; run the same command again", g.repo, err)
	}
	return g.refuse("%s was queued for %s, the newest is %s now: nothing promoted, its place is dropped; decide again whether to promote it",
		g.key(), candidateText(g.candidate), candidateText(now))
}

// candidateText names a release candidate, "" as none.
func candidateText(c string) string {
	if c == "" {
		return "no candidate"
	}
	return "candidate " + c
}

// promoteCandidate is the release candidate devctl release promote would
// dispatch for repo now (its --dry-run), "" when there is none.
var promoteCandidate = func(ctx context.Context, repo string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var stderr bytes.Buffer
	c := exec.CommandContext(ctx, merge.Tool, "release", "promote", repo, "--dry-run") //nolint:gosec // the merge tool, a checked repository
	c.Stderr = &stderr
	out, err := c.Output()
	if candidate, ok := merge.ParsePromoteCandidate(out); ok {
		return candidate, nil
	}
	if err == nil {
		err = errors.New("no document")
	}
	return "", fmt.Errorf("%s release promote %s --dry-run: %w: %s", merge.Tool, repo, err, lastOf(stderr.String()))
}

// outsideCheck is how often a merge waiting in a lane asks GitHub about the
// places before it that wait without their merge.
const outsideCheck = time.Minute

// The verbs of a merge and a promotion whose turn came, of a merge that
// merged and of a lane that settled after it.
const (
	verbMerging     = "merging"
	verbPromoting   = "promoting"
	verbMerged      = "merged"
	verbLaneSettled = "lane.settled"
)

// checkOutside settles or drops the lane's places that wait without their
// merge and whose pull request merged or closed (checkPlaces). It returns
// what the merge waits for when GitHub does not answer for a place that
// heads the lane.
func (g *gateRun) checkOutside() string {
	if err := g.checkPlaces(g.ctx, g.me, g.now, g.lane.Name, g.repo, g.pr, outsideCheck); err != nil {
		return fmt.Sprintf("lane %s waits for a place settled outside the gate, and GitHub does not answer for it (%v)", g.lane.Name, err)
	}
	return ""
}

// readRelease reads, once per merge, whether a merge into the pull
// request's base branch is tagged by its Auto-release workflow. Only a
// devctl merge waits for a release; GitHub not answering refuses.
func (g *gateRun) readRelease() error {
	if g.release != nil || g.pr == 0 || !g.runsDevctl() {
		return nil
	}
	ctx, cancel := context.WithTimeout(g.ctx, time.Minute)
	defer cancel()
	r, err := baseRelease(ctx, g.repo, g.pr)
	if err != nil {
		return g.refuse("whether %s's base branch has auto-release cannot be read (%v): run the same command again", g.key(), err)
	}
	g.release = &r
	return nil
}

// rebaseReads is how often the gate reads a pull request whose rebase GitHub
// has not computed yet.
const rebaseReads = 3

// checkRebase refuses a rebase merge GitHub reports conflicting with its
// base before it takes a lane place: devctl would refuse it only at its turn
// (exit 3, "This branch can't be rebased"), and a base merged into the
// branch hides the conflict from the pull request. GitHub not answering
// refuses; a rebase GitHub has not computed after rebaseReads reads queues,
// devctl judging it at its turn.
func (g *gateRun) checkRebase() error {
	var r github.Rebase
	for try := 1; ; try++ {
		ctx, cancel := context.WithTimeout(g.ctx, time.Minute)
		var err error
		r, err = pullRebase(ctx, g.repo, g.pr)
		cancel()
		if err != nil {
			return g.refuse("whether %s rebases onto its base cannot be read (%v): run the same command again", g.key(), err)
		}
		if r.Known || try == rebaseReads {
			break
		}
		select {
		case <-g.ctx.Done():
			return g.ctx.Err()
		case <-time.After(rebaseRetry):
		}
	}
	switch {
	case r.Conflicts():
		why := "GitHub reports it not rebaseable"
		if r.State == github.DirtyState {
			why = "it conflicts with its base (mergeable_state dirty)"
		}
		return g.refuse("%s cannot be rebased onto its base: %s; a rebase merge fails at its turn, so it takes no lane place. "+
			"Open a replacement PR (a fresh branch from the base with the change applied again; never merge the base into a branch), then merge that one",
			g.key(), why)
	case !r.Known:
		gateLine("GitHub has not computed whether %s rebases (mergeable_state %s): it queues, devctl judges the rebase at its turn", g.key(), cmp.Or(r.State, "unknown"))
	}
	return nil
}

// handCut is the base branch when no Auto-release run tags a merge into it,
// "" otherwise.
func (g *gateRun) handCut() string {
	if g.release == nil || g.release.Auto {
		return ""
	}
	return g.release.Base
}

// key names the merge, owner/repo#n or owner/repo promote.
func (g *gateRun) key() string { return state.Merge{Repo: g.repo, PR: g.pr}.Key() }

// startEvent names the command whose turn came: "merging owner/repo#n" for
// devctl pr merge, "promoting owner/repo" for devctl release promote.
func (g *gateRun) startEvent() state.Event {
	if g.pr == 0 {
		return event(g.me, verbPromoting, "%s in lane %s", g.repo, g.lane.Name)
	}
	return event(g.me, verbMerging, "%s in lane %s", g.key(), g.lane.Name)
}

// nothingDone is the outcome line's "nothing merged", "nothing promoted"
// for a promotion, with devctl's state of it when its document named one.
func nothingDone(pr int, out merge.Outcome) string {
	switch {
	case pr != 0:
		return "nothing merged"
	case out.State != "":
		return "nothing promoted (" + out.State + ")"
	}
	return "nothing promoted"
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
	return g.refuseWith(ExitGateRefused, format, args...)
}

// refuseWith is refuse with exit code code.
func (g *gateRun) refuseWith(code int, format string, args ...any) error {
	why := fmt.Sprintf(format, args...)
	g.centralLeave("refused: " + why)
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		g.drop(st)
		return []state.Event{event(g.me, "merge.refused", "%s: %s", g.key(), why)}, nil
	})
	return gateRefusedWith(code, "%s", why)
}

// laneReady reads the lane's installation: why is what the lane waits for,
// "" when it is free, which needs each of its settling merges settled. An
// installation that cannot be read refuses.
func (g *gateRun) laneReady(q merge.Lane) ([]merge.HelmRelease, string, error) {
	if g.lane.Installation == "" {
		return nil, "", nil
	}
	if st, err := g.store.Read(); err == nil {
		if h, ok := merge.FixWindow(st, g.now, g.repo, g.pr, g.lane); ok {
			gateLine("fix window: %q holds lane %s for %s (%s); it merges without %s Ready or the previous release rolled",
				h.By.Name, g.lane.Name, g.key(), h.Reason, g.lane.Installation)
			_ = g.store.Update(func(*state.State) ([]state.Event, error) {
				return []state.Event{event(g.me, "merge.window", "%s in lane %s: %q's fix window waives %s Ready and the previous release rolled (%s)",
					g.key(), g.lane.Name, h.By.Name, g.lane.Installation, h.Reason)}, nil
			})
			return nil, "", nil
		}
	}
	hrs, err := readHelmReleases(g.ctx, g.lane)
	if err != nil {
		if cause := g.teleportCause(g.ctx); cause != "" {
			return nil, "", g.refuse("lane %s cannot read the HelmReleases of %s: %s; then run the same command again", g.lane.Name, g.lane.Installation, cause)
		}
		return nil, "", g.refuse("lane %s cannot read the HelmReleases of %s (%v): log in (tsh kube login %s), then run the same command again",
			g.lane.Name, g.lane.Installation, err, g.lane.Installation)
	}
	settling := q.AllSettling
	if len(settling) == 0 {
		settling = []*state.Merge{nil}
	}
	for _, s := range settling {
		ready, why := merge.Ready(g.lane, hrs, s, g.now, g.cfg.Merge.Settle.Duration)
		switch {
		case ready:
			if why != "" {
				gateLine("lane %s: %s settles without a roll, %s", g.lane.Name, s.Key(), why)
			}
			continue
		case s != nil && g.now.Sub(s.Finished) > g.cfg.Merge.SettleTimeout.Duration:
			return nil, fmt.Sprintf("next in lane %s, stuck since %s: %s; watch says LANE STUCK, the repair is the installation's or lanes clear %s",
				g.lane.Name, s.Key(), why, g.lane.Name), nil
		}
		return nil, fmt.Sprintf("next in lane %s, waiting for %s: %s", g.lane.Name, g.lane.Installation, why), nil
	}
	return hrs, "", nil
}

// start makes the merge the lane's running one, when it still is first, its
// lane's settling merges are the ones it checked and the machine runs fewer
// than merge.cap devctl processes, and runs devctl.
func (g *gateRun) start(settling string, hrs []merge.HelmRelease) (string, error) {
	if g.runsDevctl() {
		// devctl refuses to run behind its latest release (exit 7): a merge
		// queued behind a devctl release starts on that release.
		g.installTool(g.ctx, g.me, "merge.update", "before "+g.key())
	}
	toolFrom := ""
	if g.toolMerge() {
		toolFrom = devctlVersion(g.ctx)
	}
	why := ""
	var settled []state.Merge
	err := g.store.Update(func(st *state.State) ([]state.Event, error) {
		q := merge.Queue(st, g.lane.Name)
		i := g.mine(st, state.Waiting)
		_, behind := q.Ahead(g.repo, g.pr, g.present)
		w := slices.IndexFunc(st.Holds, func(h state.Hold) bool { return h.Tool != "" && h.ToolMerged })
		switch {
		case i < 0 || behind || q.Running != nil:
			why = "the lane moved on"
			return nil, nil
		case q.SettlingKeys() != settling:
			why = "another merge of the lane just settled"
			return nil, nil
		case g.toolMerge() && w >= 0:
			h := st.Holds[w]
			why = fmt.Sprintf("next in lane %s, waiting for %s to report %s, the release of %s#%d (its window lifts then)",
				g.lane.Name, merge.Tool, h.ToolRelease, merge.ToolRepo, h.ToolPR)
			return nil, nil
		}
		if n := devctlRuns(st); n >= g.cfg.Merge.Cap {
			why = fmt.Sprintf("%d devctl processes run machine-wide (cap %d), not a lane problem: %s is next in lane %s and starts when one ends",
				n, g.cfg.Merge.Cap, g.key(), g.lane.Name)
			return nil, nil
		}
		st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool {
			if m.Lane == g.lane.Name && m.Phase == state.Settling {
				settled = append(settled, m)
				return true
			}
			return false
		})
		i = g.mine(st, state.Waiting)
		passed := q.Passed(g.repo, g.pr)
		m := &st.Merges[i]
		m.Phase, m.Started, m.Roll, m.Seeded, m.Outside = state.Running, g.now.UTC(), merge.RollSet(hrs, g.repo), false, false
		m.HandCut = g.handCut()
		m.Finished, m.Exit = time.Time{}, 0
		ev := []state.Event{g.startEvent()}
		if passed != "" {
			ev[0].Detail += ", ahead of " + passed + " (not arrived)"
		}
		if g.toolMerge() {
			st.Holds = slices.DeleteFunc(st.Holds, func(h state.Hold) bool { return h.Target == merge.AllMerges })
			h := state.Hold{Target: merge.AllMerges, Except: merge.ToolRepo, By: g.me, At: g.now.UTC(), Tool: merge.Tool, ToolFrom: toolFrom,
				ToolPR: g.pr, Reason: fmt.Sprintf("the devctl release window: %s#%d merges and every devctl run refuses until updated; the gate runs `devctl version update` once it released and lifts this once `devctl version` reports the release", g.repo, g.pr)}
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
	g.leaveCentral(g.ctx, settled, "rolled, HelmReleases of "+g.lane.Installation+" Ready")
	if g.pr == 0 {
		if err := g.checkCandidate(); err != nil {
			return "", err
		}
	}
	if g.central {
		if why, err := g.centralStart(); err != nil || why != "" {
			return why, err
		}
	}
	return "", g.runMerge()
}

// unstart puts the merge back to waiting in its local lane when its central
// lane did not let it start: it keeps its place in both.
func (g *gateRun) unstart() {
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		if i := g.mine(st, state.Running); i >= 0 {
			st.Merges[i].Phase, st.Merges[i].Started, st.Merges[i].Roll = state.Waiting, time.Time{}, nil
		}
		return nil, nil
	})
}

// runMerge runs devctl once, detached from its caller (launchChild), its
// document and exit code unchanged, and follows it to its end (follow). A
// run that does not start is recorded as one without a document.
func (g *gateRun) runMerge() error {
	defer outliveCaller()()
	if !g.runsDevctl() {
		gateLine("devctl serves the repositories of %s only (merge.devctlOwners): %s#%d takes the %s as the gh login, green first, no release wait",
			strings.Join(g.cfg.Merge.DevctlOwners, ", "), g.repo, g.pr, github.SquashRoute)
	}
	if b := g.handCut(); b != "" {
		gateLine("%s merges into %s, which no Auto-release run tags: devctl ends at the merge (--no-release-wait), awaits no release and frees lane %s then",
			g.key(), b, g.lane.Name)
	}
	base, err := g.mergeFiles()
	if err != nil {
		gateLine("%v", err)
		return g.record(base, childRun{rc: guard.ExitNotFound})
	}
	pid, rc := g.launch(base)
	if rc != 0 {
		return g.record(base, childRun{rc: rc})
	}
	return g.follow(base, pid, 0)
}

// mergeArgv is the command the merge's merge-child runs: the gated devctl
// command bounded by the gate's CI timeout, with --no-release-wait into a
// hand-cut base, or the plain squash merge for a repository devctl does not
// serve.
func (g *gateRun) mergeArgv() ([]string, error) {
	argv := g.argv
	if !g.runsDevctl() {
		self, err := selfExe()
		if err != nil {
			return nil, err
		}
		argv = squashArgv(self, g.repo, g.pr, g.argv)
	}
	if g.pr != 0 && g.runsDevctl() {
		argv = merge.CITimeout(argv, g.cfg.Merge.CITimeout.Duration)
	}
	if g.handCut() != "" {
		argv = merge.NoReleaseWait(argv)
	}
	return argv, nil
}

// launch starts the merge's merge-child (launchChild) and records it as the
// merge's child; rc is the exit to record when it does not start.
func (g *gateRun) launch(base string) (pid, rc int) {
	argv, err := g.mergeArgv()
	if err == nil {
		pid, err = launchChild(childSpec{Argv: argv, Owner: g.me, Config: g.explicitConfig(), HandCut: g.handCut()}, base)
	}
	if err != nil {
		gateLine("devctl does not start: %v", err)
		removeMergeFiles(base)
		return 0, guard.ExitNotFound
	}
	g.started(pid)
	return pid, 0
}

// serverRetries are the pauses before the gate sends a merge again whose
// merge call GitHub answered with a 5xx (merge.ServerError), one per retry.
var serverRetries = []time.Duration{10 * time.Second, 30 * time.Second, time.Minute}

// retryServerError runs the merge again while its last run ended on a 5xx
// of its merge call and GitHub reports the pull request still open: devctl
// sends the merge once, a gateway's 502 is GitHub's, and the expected head
// keeps a second call from merging anything but the judged head. A pull
// request GitHub reports otherwise, or does not answer for, is the record's
// to judge, its document set aside. It returns the last run.
func (g *gateRun) retryServerError(base string, pid int, run childRun) (childRun, int) {
	for try, pause := range serverRetries {
		if run.replaced || !merge.ServerError(run.doc) {
			return run.emit(), pid
		}
		p, err := pullState(context.WithoutCancel(g.ctx), g.repo, g.pr)
		if err != nil || p.State != github.Open {
			run = run.emit()
			run.doc = nil
			return run, pid
		}
		gateLine("GitHub answered %s's merge call with a server error (exit %d): sending it again in %s, retry %d of %d",
			g.key(), run.rc, pause, try+1, len(serverRetries))
		_ = g.store.Log(event(g.me, "merge.retry", "%s: a 5xx on the merge call, retry %d of %d", g.key(), try+1, len(serverRetries)))
		time.Sleep(pause) // the held document goes: a retry's replaces it
		var rc int
		if pid, rc = g.launch(base); rc != 0 {
			return childRun{rc: rc}, pid
		}
		run = followChild(base, pid, 0, g.bin, g.retried(try+1))
	}
	return run.emit(), pid
}

// retried is followChild's hold for a run after try retries: a document of
// a 5xx on the merge call while a retry is left.
func (g *gateRun) retried(try int) func([]byte) bool {
	if try >= len(serverRetries) {
		return nil
	}
	return merge.ServerError
}

// follow copies the merge's devctl (its merge-child pid) stderr on from
// offset to its end and records the run (record). A binary replaced
// meanwhile re-executes the installed one first (reexec), which follows the
// same devctl on from there (resumeMerge), so the installed release records
// the outcome; one that does not start is followed on under this one.
func (g *gateRun) follow(base string, pid int, offset int64) error {
	run, pid := g.retryServerError(base, pid, followChild(base, pid, offset, g.bin, g.retried(0)))
	if run.replaced {
		g.reexec("while "+g.key()+"'s devctl ran", fmt.Sprintf("%s=%d:%d", gateRunningEnv, pid, run.offset))
		run, _ = g.retryServerError(base, pid, followChild(base, pid, run.offset, nil, g.retried(0)))
	}
	defer handOver(base, run, g.cli)
	return g.record(base, run)
}

// record records the run's outcome: a merge settles its lane, one that
// warranted no release or whose lane has no installation to roll leaves it,
// and so does one with nothing merged. A run without
// its document or ended by a signal is GitHub's to judge: merged, its
// release is unconfirmed; unanswered, the lane settles by the settle rule as
// for a lost merge. The run's files go with the record; a save the newer
// release that wrote the state refuses leaves them, document and exit code,
// for the watch, which records the run from them at its next poll.
func (g *gateRun) record(base string, run childRun) error {
	doc, rc, output := run.doc, run.rc, run.kept
	note := ""
	if !g.runsDevctl() {
		note = ", " + github.SquashRoute
	}
	if output != "" {
		note += ", output in " + output
	}
	r := runOutcome{rc: rc}
	var ok bool
	if r.out, ok = parseOutcome(g.pr, doc); merge.NeedsJudging(ok, rc) {
		r.out, r.unanswered = judgeRun(g.ctx, g.repo, g.pr, judgeTries)
	}
	settles := false
	err := g.store.Update(func(st *state.State) ([]state.Event, error) {
		i := g.mine(st, state.Running)
		if i < 0 {
			return nil, nil
		}
		ev := recordRun(st, i, g.lane, g.me, r, time.Now().UTC(), note)
		settles = slices.ContainsFunc(st.Merges, func(m state.Merge) bool {
			return m.Repo == g.repo && m.PR == g.pr && m.Phase == state.Settling
		})
		return ev, nil
	})
	var stale *state.StaleWriterError
	if errors.As(err, &stale) {
		gateLine("%s's outcome is not recorded by this call, which runs beekeeper %s, older than the %s that wrote the state: its document and exit code stay in %s for the watch of the installed %s, which records the run at its next poll",
			g.key(), stale.Version, stale.Newer, filepath.Dir(base), project.Name)
	} else {
		removeMergeFiles(base)
	}
	if g.central {
		g.centralRecord(settles, fmt.Sprintf("devctl exit %d, nothing to roll", rc))
	}
	out, unanswered := r.out, r.unanswered
	if g.toolMerge() && out.Merged {
		// The release is out: install it now rather than on the watch's tick,
		// also when its window was lifted by hand, then lift the window.
		ctx := context.WithoutCancel(g.ctx)
		g.installTool(ctx, g.me, "hold.update", "after "+g.key())
		g.closeToolWindow(ctx, g.me)
	}
	switch {
	case unanswered != nil:
		gateLine("devctl ended with exit %d without its document and GitHub does not answer (%v): whether %s merged is unknown, lane %s settles by the settle rule; check the pull request, do not rerun blindly",
			rc, unanswered, g.key(), g.lane.Name)
	case out.Merged && g.handCut() != "":
		gateLine("%s merged into %s, which no Auto-release run tags: no release awaited, lane %s is free; a tag of %s is cut by hand",
			g.key(), g.handCut(), g.lane.Name, g.handCut())
	case out.Unconfirmed:
		gateLine("devctl ended with exit %d before its document, and GitHub reports %s#%d merged: its release is unconfirmed, confirm it with `devctl release wait %s --pr %d`, do not merge again",
			rc, g.repo, g.pr, g.repo, g.pr)
	case !out.Merged && (rc == devctlUsage || rc == devctlAuth):
		gateLine("%s (exit %d, a tooling fault): the same command fails the same way until what the reason names is fixed (output in %s); %s left lane %s",
			nothingDone(g.pr, out), rc, output, g.key(), g.lane.Name)
	case !out.Merged:
		gateLine("%s (exit %d); %s left lane %s: act on the reason, then run the same command again, it joins the lane anew",
			nothingDone(g.pr, out), rc, g.key(), g.lane.Name)
	case g.pr == 0:
		gateLine("promoted %s: release %s dispatched", g.repo, out.Release)
	}
	return exitCode(rc)
}

// started records the merge's devctl, which runs on when the gate is gone.
func (g *gateRun) started(pid int) {
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		if i := g.mine(st, state.Running); i >= 0 {
			st.Merges[i].Child = pid
		}
		return nil, nil
	})
}

// parseGated finds what the gate queues in argv: devctl pr merge's
// repository and number, or the one repository of devctl release promote,
// number 0.
func parseGated(argv []string) (repo string, pr int, ok bool) {
	if repo, pr, ok = merge.ParseArgs(argv); ok {
		return repo, pr, ok
	}
	repo, ok = merge.ParsePromote(argv)
	return repo, 0, ok
}

// parseOutcome reads the document of the gated command: a merge's, or a
// promotion's for pr 0.
func parseOutcome(pr int, doc []byte) (merge.Outcome, bool) {
	if pr == 0 {
		return merge.ParsePromoteDocument(doc)
	}
	return merge.ParseDocument(doc)
}

// toolMerge says whether the gate merges the merge tool's own repository,
// which opens the tool-release window.
func (g *gateRun) toolMerge() bool { return g.pr != 0 && strings.EqualFold(g.repo, merge.ToolRepo) }

// runsDevctl says whether the gated command runs devctl: a promotion, or a
// merge of a repository devctl serves; any other merge takes the plain
// squash merge.
func (g *gateRun) runsDevctl() bool { return g.pr == 0 || g.cfg.Merge.DevctlServes(g.repo) }

// judgeTries is how often the gate asks GitHub about a run without its
// document, judgeWait the pause between the tries.
const judgeTries = 3

var judgeWait = 10 * time.Second

// judgeRun asks GitHub, up to tries times, whether repo#pr merged, as its
// run left no document to say it; the caller's context may be gone, the
// gate's is not.
func judgeRun(ctx context.Context, repo string, pr, tries int) (merge.Outcome, error) {
	if pr == 0 {
		return merge.Outcome{}, errors.New("a promotion has no pull request to ask about")
	}
	ctx = context.WithoutCancel(ctx)
	var err error
	for try := range tries {
		if try > 0 {
			time.Sleep(judgeWait)
		}
		var p github.Pull
		if p, err = pullState(ctx, repo, pr); err == nil {
			return merge.Judged(p), nil
		}
	}
	return merge.Outcome{}, err
}

// runOutcome is how one merge's devctl run ended: its outcome, its exit
// code, and GitHub's error when nothing could judge it.
type runOutcome struct {
	out        merge.Outcome
	rc         int
	unanswered error
}

// recordRun records the outcome of the running merge st.Merges[i] in lane:
// a merge settles its lane, one that warranted no release or whose lane has
// no installation to roll leaves it, so does one with nothing merged (its
// place dies with its run), and one nothing could judge settles by the
// settle rule. A devctl merge's release window records the merge, or lifts
// when nothing merged. It returns the events, with note appended.
func recordRun(st *state.State, i int, lane config.Lane, by state.Party, r runOutcome, now time.Time, note string) []state.Event {
	m := &st.Merges[i]
	key, repo, pr, out, rc, handCut := m.Key(), m.Repo, m.PR, r.out, r.rc, m.HandCut
	var ev []state.Event
	switch {
	case r.unanswered != nil:
		m.Phase, m.Finished, m.Exit, m.Release, m.Roll = state.Settling, now, rc, "", nil
	case out.Merged && !out.NoRelease && handCut == "" && lane.Installation != "":
		m.Phase, m.Finished, m.Exit, m.Release = state.Settling, now, rc, out.Release
	default:
		st.Merges = slices.Delete(st.Merges, i, i+1)
	}
	if strings.EqualFold(repo, merge.ToolRepo) && pr != 0 && r.unanswered == nil {
		for j, h := range st.Holds {
			if h.Tool != "" && out.Merged {
				st.Holds[j].ToolRelease, st.Holds[j].ToolMerged = out.Release, true
			}
		}
		why := ""
		switch {
		case !out.Merged:
			why = "merged nothing"
		case out.NoRelease:
			why = "warranted no release"
		case handCut != "":
			why = "awaits no release, " + handCut + " has no auto-release"
		}
		if why != "" {
			st.Holds = slices.DeleteFunc(st.Holds, func(h state.Hold) bool {
				if h.Tool == "" {
					return false
				}
				ev = append(ev, event(by, "hold.lift", "%s: %s %s", h.Target, key, why))
				return true
			})
		}
	}
	release := out.Release
	switch {
	case handCut != "":
		release = "none awaited (" + handCut + " has no auto-release)"
	case out.NoRelease:
		release = "none warranted"
	case out.Unconfirmed:
		release = "unconfirmed (merged per GitHub)"
	case release == "":
		release = "unknown"
	}
	var e state.Event
	switch {
	case r.unanswered != nil:
		e = event(by, "merge.unknown", "%s exit %d without its document, GitHub does not answer (%v): lane %s settles by the settle rule",
			key, rc, r.unanswered, lane.Name)
	case !out.Merged:
		e = event(by, "merge.failed", "%s exit %d, %s, it left lane %s", key, rc, nothingDone(pr, out), lane.Name)
	default:
		e = event(by, verbMerged, "%s exit %d, release %s", key, rc, release)
	}
	e.Detail += note
	return append(ev, e)
}

// closeToolWindow lifts a tool-release window once no merge of the tool's
// repository runs and the tool reports another version than at its opening,
// or GitHub reports the window's pull request not merged: its merge ended
// without the gate recording it (a gate killed, GitHub unanswered). A window
// whose merge merged updates the tool (updateTool) and lifts once it reports
// the release.
func (a *app) closeToolWindow(ctx context.Context, by state.Party) {
	st, err := a.store.Read()
	if err != nil || !slices.ContainsFunc(st.Holds, func(h state.Hold) bool { return h.Tool != "" }) {
		return
	}
	if slices.ContainsFunc(st.Merges, func(m state.Merge) bool {
		return strings.EqualFold(m.Repo, merge.ToolRepo) && m.Phase == state.Running && merge.Runs(m, proc.Alive)
	}) {
		return
	}
	v := devctlVersion(ctx)
	if v != "" && a.updateTool(ctx, v, by) {
		v = devctlVersion(ctx)
	}
	pulls := map[int]github.Pull{}
	for _, h := range st.Holds {
		if h.Tool != "" && h.ToolPR != 0 && !h.ToolMerged && (v == "" || !merge.Installed(h, v)) {
			if p, err := pullState(ctx, merge.ToolRepo, h.ToolPR); err == nil {
				pulls[h.ToolPR] = p
			}
		}
	}
	if v == "" && len(pulls) == 0 {
		return
	}
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		var ev []state.Event
		st.Holds = slices.DeleteFunc(st.Holds, func(h state.Hold) bool {
			if h.Tool == "" {
				return false
			}
			if v != "" && merge.Installed(h, v) {
				ev = append(ev, event(by, "hold.lift", "%s: devctl now reports %s (the window opened on %s)", h.Target, v, h.ToolFrom))
				return true
			}
			p, ok := pulls[h.ToolPR]
			if !ok || h.ToolMerged || p.State == github.Merged {
				return false
			}
			ev = append(ev, event(by, "hold.lift", "%s: %s#%d is %s, its merge ended with nothing merged", h.Target, merge.ToolRepo, h.ToolPR, strings.ToLower(p.State)))
			return true
		})
		for i, h := range st.Holds {
			if p, ok := pulls[h.ToolPR]; ok && h.Tool != "" && p.State == github.Merged {
				st.Holds[i].ToolMerged = true
			}
		}
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
	running, children := map[int]bool{}, map[int]bool{}
	for _, m := range st.Merges {
		if m.Phase == state.Running {
			running[m.PID], children[m.Child] = true, true
		}
	}
	n := len(running)
	t, err := plat.Machine.Processes()
	if err != nil {
		return n
	}
	for _, p := range t.ByPID {
		if p.Comm == merge.Tool && !running[p.PPID] && !children[p.PPID] {
			n++
		}
	}
	return n
}

// toolUpdateEvery is how long beekeeper waits between two updates of the
// tool for one merged window.
const toolUpdateEvery = 2 * time.Minute

// updateTool runs the tool's update for a window whose merge merged while
// the tool, reporting v, does not report its release yet, at most once per
// toolUpdateEvery and window, so the window does not wait for somebody to
// install the release. A failed update is logged. It reports whether it ran
// the update.
func (a *app) updateTool(ctx context.Context, v string, by state.Party) bool {
	now := time.Now().UTC()
	due := false
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		for i, h := range st.Holds {
			if h.Tool != "" && h.ToolMerged && !merge.Installed(h, v) && now.Sub(h.ToolUpdated) >= toolUpdateEvery {
				st.Holds[i].ToolUpdated, due = now, true
			}
		}
		return nil, nil
	})
	if due {
		a.installTool(ctx, by, "hold.update", fmt.Sprintf("from %s (retried in %s)", v, toolUpdateEvery))
	}
	return due
}

// installTool runs the tool's update, which installs its latest release when
// the installed tool is behind it and does nothing otherwise; a failure is
// logged under verb.
func (a *app) installTool(ctx context.Context, by state.Party, verb, what string) {
	if err := devctlUpdate(ctx); err != nil {
		_ = a.store.Log(event(by, verb, "%s update %s failed: %v", merge.Tool, what, err))
	}
}

// toolUpdate runs `devctl version update`, which installs the newest
// release in place of the running binary.
func toolUpdate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, merge.Tool, "version", "update").CombinedOutput() //nolint:gosec // the merge tool, a constant
	if err != nil {
		return fmt.Errorf("%w: %s", err, truncate(strings.TrimSpace(string(out)), 200))
	}
	return nil
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
	out, err := proc.Command(ctx, "kubectl", "config", "get-contexts", "-o", "name").Output()
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

// readHelmReleases reads the lane installation's HelmReleases with kubectl,
// each with the range its OCIRepository follows.
func readHelmReleases(ctx context.Context, lane config.Lane) ([]merge.HelmRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	kctx, err := kubeContext(ctx, lane)
	if err != nil {
		return nil, err
	}
	out, err := kubectlList(ctx, kctx, "helmreleases.helm.toolkit.fluxcd.io")
	if err != nil {
		return nil, err
	}
	hrs, err := merge.ParseHelmReleases(out)
	if err != nil {
		return nil, err
	}
	if out, err = kubectlList(ctx, kctx, "ocirepositories.source.toolkit.fluxcd.io"); err != nil {
		return nil, err
	}
	return hrs, merge.AttachRanges(hrs, out)
}

// kubectlList is `kubectl get <resource> -A -o json` on context kctx, an
// error naming its last stderr line.
func kubectlList(ctx context.Context, kctx, resource string) ([]byte, error) {
	var stderr bytes.Buffer
	c := proc.Command(ctx, "kubectl", "--context", kctx, "get", resource, "-A", "-o", "json") //nolint:gosec // the configured context
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
			msg = msg[i+1:]
		}
		return nil, fmt.Errorf("context %s: %v: %s", kctx, err, msg)
	}
	return out, nil
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
