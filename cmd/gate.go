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
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/claude"
	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// The gate's own exit codes, apart from devctl's 1-9 and the busy machine's 75.
const (
	// DefaultGateWait is a foreground merge's wait for its turn before it
	// waits on in a run of its own; the hook gives a background one
	// BackgroundGateWait.
	DefaultGateWait    = 2 * time.Minute
	BackgroundGateWait = 30 * time.Minute
	// gateDeadlineEnv carries a waiting call's deadline across the re-exec
	// of a replaced binary.
	gateDeadlineEnv = "BEEKEEPER_GATE_DEADLINE"
)

func (a *app) gateCmd() *cobra.Command {
	var wait, limit time.Duration
	var queued bool
	var reviewedHead string
	c := &cobra.Command{
		Use:   gate.GateCmdName + " [--wait DURATION] [--limit DURATION] -- devctl pr merge|release promote|pr wait|release wait|rollout wait <args>",
		Short: "The PreToolUse hook's gate on devctl's blocking commands",
		Long: `gate is what the PreToolUse hook puts in front of every devctl pr merge and
every devctl release promote of one repository; a session never calls it. A
promotion queues and runs in its repository's lane like a merge, its stable
release settling the lane. One pinned with --version vX.Y.Z-rc.N promotes
that candidate when its turn comes, whatever was cut after it. An unpinned
one promotes the candidate newest when it was called, or nothing: a newer
one by its turn refuses it (77), and where a merge would wait on outside the
call (exit 76, below) it is refused (77) instead, naming the newest
candidate to pin. Only a hold refuses (exit 77): the repository,
its lane, "merges" or "github" held (a cluster upgrade on the lane's
installation holds it too); so do a GitHub budget unknown and a lane
installation that cannot be read. A read of the budget, the installation,
the release candidate, the base branch or the rebase that fails in transit
(a resolver error, a refused or reset connection, a timeout, a killed
kubectl, gh's "error connecting to") is sent again three times (10s, 30s,
1m20s apart), a line per retry, the merge keeping its place; the refusal
after the last try names the read's last error. The budget's read is the
exception: failing in transit on every try it is no refusal but exit 74
(unread), the merge neither judged nor queued, its line ("beekeeper gate:
unread, …") naming the failed read; the same command runs again once
GitHub answers. A rebase merge (--rebase) GitHub reports
not rebaseable or conflicting with its base is refused (77) before it takes
a lane place, the way out a replacement PR; GitHub not answering whether it
rebases refuses too. At its turn a rebase merge is read again for conflicts
and merge commits; a head only behind its base runs as it stands. The gate
never updates a branch or restarts CI to catch up. A protection requirement
for up-to-date branches is the repository owner's decision. A one-PR merge keeps a reserve of 200 GitHub calls and
can run below the budget floor while that reserve remains. Other work and
merges below that reserve are queued for the reset (exit 76, as below):
a run of its own waits for the budget and merges as below, and for
merge.resetFirst from the reset the merges queued for it go first, the oldest queued first: only the running
merges count against merge.cap for them, and the waits below start after
them; a merge marked urgent (lanes urgent)
runs under the floor instead, once per reset window. Otherwise the merge
joins its lane's queue (a merge registered with lanes settle heads it) and
runs when no merge before it holds its place (one in the gate or within
merge.queueTTL of its last run; for a seeded place also the seeds before
it), nothing else of the lane runs, the lane's HelmReleases are Ready and
the previous merge's release has rolled, fewer than merge.cap devctl
merges and promotes run (the waits only poll and do not count) and the
calling session runs fewer than merge.sessionCap
merges (default 2: each running devctl polls GitHub, a waiting place does
not; the session's merges start in the order they queued). A wait longer than --wait hands the merge to a run of its
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
whose release is older than the one that wrote the state (its save
refused, or a state it does not decode) and that cannot re-execute is
refused (77) before it touches the lane, its line naming both releases:
the installed beekeeper gates the same command again; one whose devctl already ran leaves
the run's document and exit code for the watch, which records the outcome
at its next poll. A merge into a base branch no Auto-release
run tags (the Auto-release workflow on the branch names no push to it; its
tags are cut by hand) runs devctl with --no-release-wait: its lane frees
the moment devctl reports it merged and no release is awaited. The pull
request's base branch is read once per merge (REST), how the branch
releases once per repository and branch while a place queued before that
read is still in the gate, at most an hour. GitHub refusing that read
under a spent rate limit is a wait for the limit's reset in the merge's
place, as under the floor, and a read again then; GitHub not answering for
the base otherwise refuses (77), the merge's place kept for merge.seedTTL
so the same command rejoins its lane where it was. A
base branch on the release-PR flow (zz_generated.create_release.yaml tags
the merge of the bot's <base>#release#<level> pull request) runs devctl
with --no-release-wait too: the release pull request's merge holds its lane
while the gate awaits its tag pipeline (devctl release wait), any other
merge frees it at the merge. A
SIGTERM aimed at the gate, its caller still there two seconds later,
stops devctl too, and so does a TaskStop of the background task the gate
runs, its output piped on or not (the PreToolUse hook ends its devctl
first and tells the stopping session whether the merge was cancelled or
landed before the stop). A merge call GitHub
answers with a 5xx is sent again while the pull request is open, up to
three times (10s, 30s, 1m apart); the caller reads the last document. A
merge with --update-branch whose base moved during every one of devctl's
update waits (exit 3, "after 3 waits") keeps its lane place, so no other
gated merge of the repository moves the base, and its merge-child runs
devctl again after a pause (1m, 2m, 5m, then 10m each), which updates the
branch once its checks are green, until it lands or merge.busyBudget
(default 4h from devctl's first run) is spent; each run's line names the
budget spent, and a spent budget ends the run with devctl's exit 3, its
reason naming the budget. A
red run whose every red check is let through runs again with devctl's
--accept-red for each: a check the base branch does not require (branch
protection or a ruleset), no security scan (nancy, gitleaks, govulncheck,
trivy, CodeQL, ...), and red on the base branch's head too; one green, still
running or without a result there, any required check and any security scan
end the run red as before, the gate's line naming why. An installed devctl
without the flag ends it red too, the line saying the merge waits for a
devctl release that has it. A run
with nothing merged leaves the lane with its run, stays on it as a failed
attempt with its reason (lanes) until the pull request's next attempt, and
wakes its owner with devctl's verdict and the failed checks, also while the
caller listens. A run without its
document or ended by a signal is judged by GitHub: merged, its release is
unconfirmed. A second merge of a pull request whose merge runs is refused
(exit 3) with that run's start, owner and last line.

A change that needs two pull requests to land together (two repositories
of one installation) names the other on each merge: devctl pr merge
<owner/repo> <n> --pair <owner/repo#m>, a flag the gate takes off before
devctl runs. Each merge keeps its place in its own lane; the first to reach
its turn waits there ("it lands with …") until the other is next in its lane
too, then starts, taking two merge.cap slots (and two of the party's
merge.sessionCap) for both, and the other starts at its next check outside
the caps. A refusal of either before its devctl runs (a hold on a new
merge, a reviewer, a rebase conflict, the end of a queued run's wait)
refuses the other with the same reason, waiting or arriving later; a run
that merged nothing is no refusal, and the other merges on. A merge pairs
with one other at most: a second --pair naming another is refused. lanes
shows each place with the merge it lands with and where that is.

devctl pr wait, release wait and rollout wait run the same way outside
their caller, without a queue, at the budget's reset after the merges
queued for it (merge.resetFirst); one whose identical command already runs on
the machine follows that run (follow-run: its stderr, document and exit
code) instead of polling GitHub a second time. Whichever command it is, its outcome reaches
the session that started it: the caller sees the output and exit code as
ever while it listens, and when it no longer does (a merge whose caller
was killed, a headless turn that ended, its CLI gone, a queued merge) the
run wakes its owner, its session on the roster or off it, with one line: "<command> exit N:
<reason> (output in <file>)", logged as devctl.unheard. A wait ends with
its caller instead: its shell stopped (TaskStop), killed or gone with an
ended turn, its devctl is stopped within a second and nobody is woken,
unless another run of the same command follows it; a wait the call leaves
(merge.toolLimit, the caller's timeout, below) runs on and wakes its owner
as a merge does. A run stopped by a signal
before its command settled (a cleanup's SIGTERM, a stop of its unit) wakes
its owner with the stop instead, as nothing waits for the command any more;
a merge stopped so (its user manager going down with a desktop restart),
or whose devctl a signal ended without its document, wakes it with what
GitHub says of the pull request: merged, open or closed. A merge cut before
any merge-child wrote devctl's exit code (killed with the machine or the
desktop: its gate and merge-child gone, running or a queued merge's own run
still waiting) is recorded as cut (merge.cut, the watch's MERGE CUT line)
at the watch's next poll after the start, and its owner woken the same way,
once.
An owner off the roster is woken by its session id, as agents wake does; one
whose session is gone (no transcript, no harness process) gets no wake and
one devctl.unheard line, and the issues it served go back to the board. The
outcome is owed in the state before it is handed over: one whose merge-child was killed on the
way, whose pull request GitHub did not answer for or whose wake the service
manager refused stays owed, and the watch hands it over once it can (a try
a minute, GitHub's answer awaited 30 minutes, given up after a day).

The gate's stdout is devctl's document and nothing else: the gate's own
lines (prefixed "beekeeper gate: ") and devctl's progress (waiting for
image …) go to stderr, so "<command> | jq ." parses; a 2>&1 before the
pipe mixes the progress into the document.

A desktop Bash call stops its command at 10 minutes without an exit code.
A foreground merge, promotion or wait therefore leaves merge.toolLimit
(default 9m) after the call started, whether it still waits for its turn or its
devctl runs: exit 76 as for a queued merge, devctl (or the wait, in a run of
its own) goes on outside the call, and its outcome wakes the owner as
above; do not run it again, do not poll. A merge expected to outlive the
limit runs with run_in_background, which the hook gives --limit 0 (no
limit) and a 30m wait. A timeout wrapped around the call (timeout 600 devctl
…, coreutils' timeout, which runs the call as its child; busybox's execs it
and is not seen) cuts the connection, not the run: its SIGTERM ends the call at once with
exit 76, devctl goes on outside it and its outcome wakes the owner as above.
A gated command is therefore never wrapped in timeout.

gate runs lists the runs the gate runs outside their callers, gate stop
<id> ends one and its devctl: never a kill by pid.`,
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		PersistentPreRunE: func(_ *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				if _, _, ok := parseGated(args); ok {
					return gate.GateRefused("the configuration does not load (%v): fix it, then run the same command again", err)
				}
				gate.GateLine("the configuration does not load (%v): this runs unowned, its outcome reaches nobody if your turn ends first", err)
				a.store = nil
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("limit") {
				limit = a.cfg.Merge.ToolLimit.Duration
			}
			return a.gate(cmd.Context(), args, wait, limit, queued, reviewedHead)
		},
	}
	c.Flags().SetInterspersed(false)
	c.Flags().DurationVar(&wait, "wait", DefaultGateWait, "how long to wait for the merge's turn")
	c.Flags().DurationVar(&limit, "limit", 0, "how long the call runs before it exits 76 and the merge or wait runs on outside it (default merge.toolLimit; 0: no limit)")
	c.Flags().StringVar(&reviewedHead, "reviewed-head", "", "pin the supervisor merge to its reviewed head; refuse a moved head")
	c.Flags().BoolVar(&queued, "queued", false, "wait as a queued merge's own run, which refuses at the end of --wait")
	_ = c.Flags().MarkHidden("queued")
	c.AddCommand(a.gateRunsCmd(), a.gateStopCmd(), a.gateCheckCmd())
	return c
}

func (a *app) gate(ctx context.Context, argv []string, wait, limit time.Duration, queued bool, reviewedHead ...string) error {
	if inSandbox() && len(reviewedHead) > 0 && reviewedHead[0] != "" {
		return refused("reviewed merges run on the coordinator, outside an execution sandbox")
	}
	if inSandbox() {
		return a.gateBrokered(argv, wait, queued)
	}
	argv, detached := merge.StripDetach(argv)
	if len(detached) > 0 {
		gate.GateLine("dropped %s: the gate runs the merge outside your session already, its outcome reaches you as for the blocking form",
			strings.Join(detached, " "))
	}
	argv, pair, paired := merge.StripPair(argv)
	repo, pr, ok := parseGated(argv)
	if !ok {
		if a.store != nil && merge.ParseOwned(argv) {
			return a.ownedRun(ctx, argv, limit)
		}
		return gate.ExitCode(runChild(argv, os.Stdout))
	}
	if err := a.checkRepositoryVisibility(ctx, repo); err != nil {
		return gate.GateRefused("%s", err)
	}
	if pr != 0 && a.cfg.Merge.Methods[repo] == mergeMethodRebase {
		argv = merge.RequireRebase(argv)
	}
	me := a.gateCaller(repo, pr, argv, queued)
	g := &gate.GateRun{Host: a, Ctx: ctx, Argv: argv, Repo: repo, Pr: pr, Lane: a.cfg.LaneOf(repo), Me: me, Pid: os.Getpid(), Cli: callerCLI(), Queued: queued,
		Bin: runningBinary(), Output: gateOutput()}
	if len(reviewedHead) > 0 {
		g.ReviewedHead = reviewedHead[0]
	}
	if err := g.CheckMergeRoute(); err != nil {
		return err
	}
	g.Central = pr != 0 && a.cfg.CentralLane(g.Lane)
	g.LeaveAt = leaveAt(clk.Now(), limit, queued)
	if paired {
		a.now = clk.Now()
		if err := g.DeclarePair(pair); err != nil {
			return err
		}
	}
	resumed := false
	if v, ok := os.LookupEnv(gate.GateRunningEnv); ok {
		_ = os.Unsetenv(gate.GateRunningEnv)
		// A merge a hold took back out of its lane waits on in its place.
		if err := g.ResumeMerge(v); !errors.As(err, new(*gate.PreemptedError)) {
			return err
		}
		resumed = true
	}
	// A queued run takes over the place its gate holds: none means dropped.
	g.Placed = queued || resumed
	if pr == 0 {
		_, pin, _ := merge.ParsePromote(argv)
		switch {
		case pin != "" && !strings.HasPrefix(pin, "v"):
			return gate.GateRefusedWith(3, "invalid promotion version %q: expected vX.Y.Z (for example v1.2.3 or v1.2.3-rc.1); nothing promoted", pin)
		case pin != "":
			g.Candidate, g.CandidateRead, g.Pinned = pin, true, true
		case !queued:
			if err := g.ReadRetrying("the release candidate of "+repo, func() (err error) {
				g.Candidate, err = gate.PromoteCandidate(ctx, repo)
				return err
			}); err != nil {
				return gate.GateRefused("the release candidate of %s is unknown (%v): nothing promoted; run the same command again", repo, err)
			}
			g.CandidateRead = true
		}
	}
	if v, ok := os.LookupEnv(gate.GateFromEnv); ok {
		_ = os.Unsetenv(gate.GateFromEnv)
		g.From, _ = strconv.Atoi(v)
	}
	// A fresh rebase merge, not one that has its place already (its queued
	// run, a re-executed wait), is judged before it takes a lane place.
	if _, waiting := os.LookupEnv(gateDeadlineEnv); !waiting && !queued && !resumed && merge.Rebase(argv) {
		if err := g.CheckRebase(); err != nil {
			return err
		}
	}
	// A fresh merge a reviewer holds takes no lane place.
	if _, waiting := os.LookupEnv(gateDeadlineEnv); !waiting && !queued && !resumed {
		if err := g.CheckReviews(g.RefuseWith); err != nil {
			return err
		}
	}
	deadline := clk.Now().Add(wait)
	if v, ok := os.LookupEnv(gateDeadlineEnv); ok {
		_ = os.Unsetenv(gateDeadlineEnv) // devctl must not inherit it
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			deadline = t
			gate.GateLine("continuing under %s %s: %s keeps its place in lane %s", project.Name, project.Version(), g.Key(), g.Lane.Name)
		}
	}
	if !g.LeaveAt.IsZero() && g.LeaveAt.Before(deadline) {
		deadline = g.LeaveAt
	}
	pause := time.Duration(0)
	for {
		a.now = clk.Now()
		// Before the step saves: the new release may have written the
		// state, which refuses this binary's save.
		if g.Bin.Replaced() {
			g.Reexec("while the merge waited", gateDeadlineEnv+"="+deadline.Format(time.RFC3339Nano))
		}
		why, err := g.Step()
		var stale *state.StaleWriterError
		if errors.As(err, &stale) {
			// The step saved nothing: a replaced binary carries on as the
			// installed one, with the same place.
			if g.Bin.Replaced() {
				g.Reexec("while the merge waited", gateDeadlineEnv+"="+deadline.Format(time.RFC3339Nano))
			}
			return g.StaleRefused(stale)
		}
		if err != nil || why == "" {
			return err
		}
		if g.Requeued {
			// A merge a hold took back out of its lane waits its turn anew.
			g.Requeued = false
			if deadline = clk.Now().Add(wait); !g.LeaveAt.IsZero() && g.LeaveAt.Before(deadline) {
				deadline = g.LeaveAt
			}
		}
		if !a.now.Before(deadline) {
			if g.Queued {
				return g.Refuse("waited %s for its turn, %s; its place is dropped: run the same command again once the lane moves", wait, why)
			}
			return g.Enqueue(why)
		}
		if why != g.LastWhy {
			gate.GateLine("waiting (up to %s): %s", deadline.Sub(a.now).Round(time.Second), why)
			g.LastWhy = why
		}
		pause = gateQueuePause(pause, g.Passive)
		if g.Central && gateApp(g.Host).cfg.Merge.QueueTTL.Duration > 0 {
			pause = min(pause, gateApp(g.Host).cfg.Merge.QueueTTL.Duration/2)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-clk.After(min(clk.Until(deadline), pause)):
		}
	}
}

// leaveAt is when a call started at now leaves its merge to run on outside
// it: a re-executed call's own time (gate.GateLimitEnv), else now+limit; never for
// a queued merge's own run, which no tool call bounds, nor without a limit.
func leaveAt(now time.Time, limit time.Duration, queued bool) time.Time {
	v, carried := os.LookupEnv(gate.GateLimitEnv)
	_ = os.Unsetenv(gate.GateLimitEnv) // devctl must not inherit it
	switch {
	case queued:
		return time.Time{}
	case carried:
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t
		}
	}
	if limit <= 0 {
		return time.Time{}
	}
	return now.Add(limit)
}

// The verbs of a merge and a promotion whose turn came, of a merge that
// merged and of a lane that settled after it.
const (
	verbMerged      = config.SinkMerged
	verbLaneSettled = "lane.settled"
)

// gateCaller resolves the session or agent asking for a gated command when
// standard caller env vars are absent (for example in a detached or queued
// child or a subprocess whose environment was stripped).
func (a *app) gateCaller(repo string, pr int, argv []string, queued bool) state.Party {
	if me, err := a.caller(); err == nil {
		return me
	}
	if a.store != nil {
		if queued {
			if from, err := strconv.Atoi(os.Getenv(gate.GateFromEnv)); err == nil && from > 0 {
				if info, ok := gate.ReadRunInfo(ownedBase(a.store.Dir(), argv, from)); ok && info.Owner.Name != "" && !strings.HasPrefix(info.Owner.Name, "pid ") {
					return info.Owner
				}
			}
		}
		if st, err := a.store.Read(); err == nil {
			if repo != "" {
				for _, m := range st.Merges {
					if m.Repo == repo && m.PR == pr && m.By.Name != "" && !strings.HasPrefix(m.By.Name, "pid ") {
						return m.By
					}
				}
			}
			if loopPID := a.loopPID(); loopPID > 0 && os.Getppid() == loopPID {
				return state.Party{Name: loop.LoopParty.Name, Person: a.cfg.Identity.Person, Team: a.cfg.Identity.Team, Host: a.cfg.Identity.Host}
			}
			sessions, t, serr := a.sessions()
			if serr == nil && t != nil {
				loopPID := a.loopPID()
				byPID := make(map[int]*claude.Session, len(sessions))
				for _, s := range sessions {
					byPID[s.PID] = s
				}
				envKeys := []string{
					orphanSessionEnv, "CLAUDE_CODE_HOST_SESSION_ID", "CLAUDE_CODE_SESSION_NAME",
					omp.EnvAgent, omp.EnvName,
					agy.EnvAgent, agy.EnvName,
					codex.EnvAgent, codex.EnvName,
				}
				for _, anc := range t.Ancestors(os.Getpid()) {
					if loopPID > 0 && anc.PID == loopPID {
						return state.Party{Name: loop.LoopParty.Name, Person: a.cfg.Identity.Person, Team: a.cfg.Identity.Team, Host: a.cfg.Identity.Host}
					}
					if s, ok := byPID[anc.PID]; ok {
						p := s.Party()
						p.Person, p.Team, p.Host = a.cfg.Identity.Person, a.cfg.Identity.Team, a.cfg.Identity.Host
						return p
					}
					if _, env, err := a.requesterOrigin(anc.PID, envKeys); err == nil && len(env) > 0 {
						getenv := func(k string) string {
							prefix := k + "="
							for _, kv := range env {
								if v, ok := strings.CutPrefix(kv, prefix); ok {
									return v
								}
							}
							return ""
						}
						p := envParty(getenv)
						if p.Name != "" || p.Session != "" || p.HostSession != "" {
							if p.Name == "" && p.HostSession != "" {
								if r, ok := claude.ReadRecord(a.cfg, p.HostSession); ok && r.Title != "" {
									p.Name = r.Title
								}
							}
							if p.Name == "" {
								for _, ag := range st.Agents {
									if ag.Is(p) && ag.Name != "" {
										p.Name = ag.Name
										break
									}
								}
							}
							if p.Name == "" {
								p.Name = cmp.Or(p.Session, p.HostSession)
							}
							p.Person, p.Team, p.Host = a.cfg.Identity.Person, a.cfg.Identity.Team, a.cfg.Identity.Host
							return p
						}
					}
				}
			}
		}
	}
	return state.Party{Name: fmt.Sprintf("pid %d", os.Getppid())}
}

// parseGated finds what the gate queues in argv: devctl pr merge's
// repository and number, or the one repository of devctl release promote,
// number 0.
func parseGated(argv []string) (repo string, pr int, ok bool) {
	if repo, pr, ok = merge.ParseArgs(argv); ok {
		return repo, pr, ok
	}
	repo, _, ok = merge.ParsePromote(argv)
	return repo, 0, ok
}

// recordRun records the outcome of the running merge st.Merges[i] in lane:
// a merge settles its lane, one that warranted no release or whose lane has
// no installation to roll leaves it (in that lane once its release is known),
// so does one with nothing merged (its
// place dies with its run), and one nothing could judge settles by the
// settle rule. A merge settles from GitHub's merge time when it is recorded
// after it, from now otherwise. A devctl merge's or promotion's release
// window records the release, or lifts when nothing merged, when the release is a candidate (no
// update installs it) or when a promotion ended without its document. It returns the events, with note appended.
func recordRun(st *state.State, i int, lane config.Lane, by state.Party, r gate.RunOutcome, now time.Time, note string) []state.Event {
	m := &st.Merges[i]
	key, repo, pr, out, rc, handCut := m.Key(), m.Repo, m.PR, r.Out, r.Rc, m.HandCut
	ev := recordDeliveryRelease(st, repo, pr, out, handCut, now)
	// The detached gate, rather than its submitting loop, owns the actual
	// merge outcome. Record it atomically with the merge's terminal event.
	if m.By.Name == loop.LoopParty.Name && pr > 0 && (out.Merged || r.Unanswered != nil || st.Stop == nil && rc != github.SquashRed && rc != github.SquashNotApplicable && rc != github.SquashRefused && rc != gate.ExitGateRefused && rc != gate.ExitGateUnread) {
		reason := fmt.Sprintf("merge-command-exit-%d", rc)
		if out.Merged {
			reason = ""
		}
		o := throughputOutcome{At: now, Operation: runMerge, Reason: reason, Candidate: loop.NormalizeLoopVersion(loop.LoopRunningVersion()), URL: publicPullURL(fmt.Sprintf("https://github.com/%s/pull/%d", repo, pr)), Success: out.Merged, Unknown: r.Unanswered != nil}
		b, _ := json.Marshal(o)
		ev = append(ev, event(loop.LoopParty, throughputOutcomeVerb, "%s", b))
	}
	switch {
	case out.Merged:
		merge.Retry(st, repo, pr)
	case r.Unanswered == nil:
		merge.Fail(st, state.Failed{Repo: repo, PR: pr, Head: m.Head, Lane: lane.Name, By: m.By, At: now, Exit: rc, Reason: r.Reason})
	}
	switch {
	case r.Unanswered != nil:
		m.Phase, m.Finished, m.Exit, m.Release, m.Roll = state.Settling, now, rc, "", nil
	case out.Merged && !out.NoRelease && handCut == "" && (lane.Installation != "" || out.Release == ""):
		m.Phase, m.Finished, m.Exit, m.Release, m.ReleaseFailed = state.Settling, out.SettlesFrom(now), rc, out.Release, out.ReleaseFailed
	default:
		st.Merges = slices.Delete(st.Merges, i, i+1)
	}
	if strings.EqualFold(repo, merge.ToolRepo) && (r.Unanswered == nil || pr == 0) {
		for j, h := range st.Holds {
			if h.Tool != "" && out.Merged {
				st.Holds[j].ToolRelease, st.Holds[j].ToolMerged = out.Release, true
			}
		}
		why := ""
		switch {
		case r.Unanswered != nil:
			why = "ended without its document, no pull request to ask: the next devctl run updates devctl first"
		case !out.Merged:
			why = "merged nothing"
			if pr == 0 {
				why = "promoted nothing"
			}
		case out.NoRelease:
			why = "warranted no release"
		case handCut != "":
			why = "awaits no release, " + handCut + " has no auto-release"
		case merge.PreRelease(out.Release):
			why = "released the candidate " + out.Release + ", which `devctl version update` never installs: its promotion opens the window anew"
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
		release = gate.NoneWarranted
	case out.ReleaseFailed && release != "":
		release += " failed, nothing rolls"
	case out.ReleaseFailed:
		release = "failed, nothing rolls"
	case out.Unconfirmed:
		release = "unconfirmed (merged per GitHub)"
	case r.Confirming:
		release = "being confirmed, its verdict follows as merge.release"
	case release == "":
		release = "unknown"
	}
	var e state.Event
	switch {
	case r.Unanswered != nil:
		e = event(by, "merge.unknown", "%s exit %d without its document, GitHub does not answer (%v): lane %s settles by the settle rule",
			key, rc, r.Unanswered, lane.Name)
	case !out.Merged:
		e = event(by, "merge.failed", "%s exit %d, %s, it left lane %s", key, rc, gate.NothingDone(pr, out), lane.Name)
		if r.Reason != "" {
			e.Detail += ": " + r.Reason
		}
	default:
		e = about(event(by, verbMerged, "%s exit %d, release %s", key, rc, release), key)
	}
	e.Detail += note
	return append(ev, e)
}

// toolCheckEvery is how long the gates and the watch wait between two reads
// of an open tool-release window's state: the version the tool reports and,
// for a window whose merge is not known merged, its pull request on GitHub.
const toolCheckEvery = 30 * time.Second

// pollToolWindow is closeToolWindow for a waiting gate step and a watch
// poll, which all run every few seconds while a window is open: of all of
// them one reads the window's state per toolCheckEvery, the read claimed in
// the state (ToolChecked) under its lock before it is made, so n queued
// merges read the window's pull request once per interval, not n times per
// step. The gate whose run of the window's merge ended calls closeToolWindow
// at once instead.
func (a *app) pollToolWindow(ctx context.Context, by state.Party) {
	st, err := a.store.Peek()
	if err != nil || !toolWindowOpen(st) {
		return
	}
	now := clk.Now().UTC()
	due := func(h state.Hold) bool { return h.Tool != "" && now.Sub(h.ToolChecked) >= toolCheckEvery }
	if !slices.ContainsFunc(st.Holds, due) {
		return
	}
	claimed := false
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		for i, h := range st.Holds {
			if due(h) {
				st.Holds[i].ToolChecked, claimed = now, true
			}
		}
		return nil, nil
	})
	if err == nil && claimed {
		a.closeToolWindow(ctx, by)
	}
}

// toolWindowOpen says a tool-release window is open.
func toolWindowOpen(st *state.State) bool {
	return slices.ContainsFunc(st.Holds, func(h state.Hold) bool { return h.Tool != "" })
}

// toolMergeRuns says a merge or promotion of the tool's repository runs: its
// devctl may still wait for its release after the release is out.
func toolMergeRuns(st *state.State) bool {
	return slices.ContainsFunc(st.Merges, func(m state.Merge) bool {
		return strings.EqualFold(m.Repo, merge.ToolRepo) && m.Phase == state.Running && merge.Runs(m, proc.Alive)
	})
}

// closeToolWindow lifts a tool-release window once the tool reports its
// release, the moment the release's binary assets exist and verify: a
// window whose pull request merged, or a promotion's, runs the tool's update
// (updateTool) also while the merge's devctl still waits for the release
// workflow, and lifts once the tool reports the release tagged on the merge
// commit (or a later release). Once no merge of the
// tool's repository runs, a window whose pull request GitHub reports not
// merged lifts too: its merge ended without the gate recording it (a gate
// killed, GitHub unanswered). It reads the tool and GitHub every time: a
// poll goes through pollToolWindow.
func (a *app) closeToolWindow(ctx context.Context, by state.Party) {
	st, err := a.store.Read()
	if err != nil || !toolWindowOpen(st) {
		return
	}
	running := toolMergeRuns(st)
	v := gate.DevctlVersion(ctx)
	pulls := map[int]github.Pull{}
	for _, h := range st.Holds {
		if h.Tool != "" && h.ToolPR != 0 && (!h.ToolMerged || h.ToolRelease == "") {
			if p, err := gate.PullState(ctx, merge.ToolRepo, h.ToolPR); err == nil {
				pulls[h.ToolPR] = p
			}
		}
	}
	releases := map[int]string{}
	for pr, p := range pulls {
		if p.State == github.Merged {
			if tag, err := toolRelease(ctx, p.MergeCommit.OID); err == nil {
				releases[pr] = tag
			}
		}
	}
	if slices.ContainsFunc(st.Holds, func(h state.Hold) bool { return pulls[h.ToolPR].State == github.Merged }) {
		_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
			for i, h := range st.Holds {
				if p, ok := pulls[h.ToolPR]; ok && h.Tool != "" && p.State == github.Merged {
					st.Holds[i].ToolMerged = true
					if h.ToolRelease == "" {
						st.Holds[i].ToolRelease = releases[h.ToolPR]
					}
				}
			}
			return nil, nil
		})
	}
	if v != "" && a.updateTool(ctx, v, by) {
		v = gate.DevctlVersion(ctx)
	}
	if current, err := a.store.Read(); err == nil {
		st = current
	}
	candidates := publishedCandidates(ctx, st, pulls)
	if v == "" && len(candidates) == 0 && (running || len(pulls) == 0) {
		return
	}
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		running := toolMergeRuns(st)
		var ev []state.Event
		st.Holds = slices.DeleteFunc(st.Holds, func(h state.Hold) bool {
			switch {
			case h.Tool == "":
				return false
			case candidates[h.ToolPR] != "" && awaitsCandidate(h, pulls):
				detail := fmt.Sprintf("%s: %s released the candidate %s, its tag published with its binaries, which `devctl version update` never installs: its promotion opens the window anew", h.Target, gate.ToolKey(h), candidates[h.ToolPR])
				if running {
					detail += "; " + gate.ToolKey(h) + "'s devctl still waits for the release workflow"
				}
				ev = append(ev, event(by, "hold.lift", "%s", detail))
				return true
			case v != "" && merge.Installed(h, v) && (!running || merge.ToolReleased(h)):
				detail := fmt.Sprintf("%s: devctl now reports %s (the window opened on %s)", h.Target, v, h.ToolFrom)
				if running {
					detail += ", while " + gate.ToolKey(h) + "'s devctl still waits for the release workflow"
				}
				ev = append(ev, event(by, "hold.lift", "%s", detail))
				return true
			case running:
				return false
			}
			p, ok := pulls[h.ToolPR]
			if !ok || h.ToolMerged || p.State == github.Merged {
				return false
			}
			ev = append(ev, event(by, "hold.lift", "%s: %s#%d is %s, its merge ended with nothing merged", h.Target, merge.ToolRepo, h.ToolPR, strings.ToLower(p.State)))
			return true
		})
		return ev, nil
	})
}

// awaitsCandidate says the tool-release window h waits on a merge that may
// have released only a candidate: its pull request merged (pulls holds the
// read of this pass), its release not yet recorded or recorded a candidate.
func awaitsCandidate(h state.Hold, pulls map[int]github.Pull) bool {
	merged := h.ToolMerged || pulls[h.ToolPR].State == github.Merged
	return h.Tool != "" && h.ToolPR != 0 && merged && (h.ToolRelease == "" || merge.PreRelease(h.ToolRelease))
}

// publishedCandidates are, by pull request, the release candidates of the
// tool published since their window opened with this platform's binary,
// for the windows that await one (awaitsCandidate): a merge's release is
// a candidate no update installs, so the window lifts on its tag and its
// binaries, not on what devctl reports. A failed read finds none; the next
// poll asks again.
func publishedCandidates(ctx context.Context, st *state.State, pulls map[int]github.Pull) map[int]string {
	found := map[int]string{}
	for _, h := range st.Holds {
		if !awaitsCandidate(h, pulls) || h.ToolRelease == "" {
			continue
		}
		if c, err := toolCandidate(ctx, h.At); err == nil && c != "" && merge.Bare(c) == merge.Bare(h.ToolRelease) {
			found[h.ToolPR] = c
		}
	}
	return found
}

// readBudget reads the GitHub budget afresh (probeBudget); a seam for the
// tests.
var readBudget = (*app).probeBudget

// toolUpdateEvery is how long beekeeper waits between two updates of the
// tool for one window whose release is coming.
const toolUpdateEvery = time.Minute

// updateTool runs the tool's update for a window whose release is coming
// (merge.ToolReleased) while the tool, reporting v, does not report it yet, at
// most once per toolUpdateEvery and window, so the window lifts within
// minutes of the release's assets and does not wait for somebody to install
// the release. The update installs only a release whose binary verifies
// against its signature bundle: a failed one is logged and kept on the
// window (ToolFailed), which stays, a successful one clears it. It reports
// whether it ran the update.
func (a *app) updateTool(ctx context.Context, v string, by state.Party) bool {
	now := clk.Now().UTC()
	due := false
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		for i, h := range st.Holds {
			if merge.ToolReleased(h) && !merge.Installed(h, v) && now.Sub(h.ToolUpdated) >= toolUpdateEvery {
				st.Holds[i].ToolUpdated, due = now, true
			}
		}
		return nil, nil
	})
	if !due {
		return false
	}
	err := a.installTool(ctx, by, "hold.update", fmt.Sprintf("from %s (retried in %s)", v, toolUpdateEvery))
	failed := ""
	if err != nil {
		failed = truncate(err.Error(), 200)
	}
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		for i, h := range st.Holds {
			if h.Tool != "" && h.ToolUpdated.Equal(now) {
				st.Holds[i].ToolFailed = failed
			}
		}
		return nil, nil
	})
	return true
}

// installTool runs the tool's update, which installs its latest release when
// the installed tool is behind it and does nothing otherwise; a failure is
// logged under verb and returned.
func (a *app) installTool(ctx context.Context, by state.Party, verb, what string) error {
	err := devctlUpdate(ctx)
	if err != nil {
		_ = a.store.Log(event(by, verb, "%s update %s failed: %v", merge.Tool, what, err))
	}
	return err
}

// toolUpdate runs `devctl version update`, which installs the newest
// release in place of the running binary.
func toolUpdate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, merge.Tool, "version", "update") //nolint:gosec // the merge tool, a constant
	c.Env = devctlCommandEnv("")
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, truncate(strings.TrimSpace(string(out)), 200))
	}
	return nil
}

// runChild runs the command with this process's stdin and stderr, its
// stdout into out, forwarding signals, and returns its exit code.
func runChild(argv []string, out io.Writer) int {
	started := clk.Now()
	path, err := exec.LookPath(argv[0])
	if err != nil {
		gate.GateLine("%v", err)
		return guard.ExitNotFound
	}
	c := exec.Command(path, argv[1:]...) //nolint:gosec // running the caller's command is the purpose
	c.Env = devctlCommandEnv("")
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, out, os.Stderr
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	if err := c.Start(); err != nil {
		gate.GateLine("%v", err)
		return guard.ExitNotFound
	}
	go func() {
		for s := range sig {
			_ = c.Process.Signal(s)
		}
	}()
	_ = c.Wait()
	return childExit(c.ProcessState, os.Stderr, started)
}

// childExit preserves signal deaths as shell exit codes and reports how long
// the command ran, including when a caller or the watch stopped it.
func childExit(ps *os.ProcessState, errs io.Writer, started time.Time) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		_, _ = fmt.Fprintf(errs, "%sended by %s after %s\n", gate.GatePrefix, signalName(ws.Signal()), clk.Since(started).Round(time.Millisecond))
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}
