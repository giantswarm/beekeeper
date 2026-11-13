package gate

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// ExitGateQueued: the merge waits on for its turn, or for the GitHub
// budget's reset, in a run of its own, which wakes its owner with the
// outcome.
const ExitGateQueued = 76

// ExitGateRefused: a hold, a GitHub budget unknown or an unreadable
// installation; nothing is queued.
const ExitGateRefused = 77

// ExitGateUnread: the GitHub budget read failed in transit on every try
// (sysexits' EX_IOERR); the merge was neither judged nor queued, the same
// command runs again once GitHub answers.
const ExitGateUnread = 74

// ExitGateDuplicate: the pull request's merge already runs, devctl's
// "not applicable".
const ExitGateDuplicate = 3

// GatePrefix starts every line the gate prints.
const GatePrefix = "beekeeper gate: "

// GateCmdName is the gate's command, which a queued run and a brokered
// gate run, and which re-executes a replaced binary by itself.
const GateCmdName = "gate"

// GateRunningEnv carries a running merge's devctl across the re-exec of
// a replaced binary: its merge-child's pid and how far its stderr was
// copied, "<pid>:<offset>".
const GateRunningEnv = "BEEKEEPER_GATE_RUNNING"

// GateFromEnv names the gate a queued run took the merge's place from.
const GateFromEnv = "BEEKEEPER_GATE_FROM"

// GateLimitEnv carries a call's tool limit (merge.toolLimit) across the
// re-exec of a replaced binary: the time the call leaves, RFC 3339.
const GateLimitEnv = "BEEKEEPER_GATE_LIMIT"

func GateLine(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, GatePrefix+format+"\n", args...)
}

func GateRefused(format string, args ...any) error {
	return GateRefusedWith(ExitGateRefused, format, args...)
}

// GateRefusedWith prints the gate's line and exits code: "refused, …", or
// "unread, …" for a read that failed in transit, which judged nothing.
func GateRefusedWith(code int, format string, args ...any) error {
	word := "refused, "
	if code == ExitGateUnread {
		word = "unread, "
	}
	GateLine(word+format, args...)
	return Cmd.NewExitError(code, "", nil)
}

// GateRun is one gated merge.
type GateRun struct {
	ReviewedHead string
	Host         Host
	Ctx          context.Context
	Argv         []string
	Repo         string
	Pr           int
	Lane         config.Lane
	Me           state.Party
	Pid          int
	Cli          int // the caller's CLI (callerCLI)
	LastWhy      string
	seeded       bool // the merge's place was queued on the session's behalf
	Queued       bool // the merge's own run, which waits on after exit 76
	From         int  // the gate a queued run took the merge's place from
	// placed says the merge had its place in the lane: a step that finds
	// none, its place was taken out (lanes drop).
	Placed bool
	// passive says a live predecessor owns the lane's checks. This step
	// only read the queue and may sleep longer without refreshing its place.
	Passive bool
	// candidate is the release candidate a promotion is for; read says this
	// call read it (a fresh promote), else it is its place's.
	Candidate     string
	CandidateRead bool
	// pinned says the promotion names its candidate (--version).
	Pinned bool
	// central says the merge's lane queues in the central instance: joined
	// once the merge has its place there, centralWhy what it waits for
	// there as of centralAsked.
	Central      bool
	joined       bool
	centralWhy   string
	CentralAsked time.Time
	// release is how the pull request's base branch releases, read once
	// before devctl's turn.
	Release *github.BaseRelease
	// releaseReset is the reset of the GitHub limit that refused the read
	// of release: until then the merge waits in its place, read no more.
	releaseReset time.Time
	// bin is the executable this call runs, re-executed once replaced.
	Bin Binary
	// output is the file this call's stdout writes to (gateOutput), the
	// background task a TaskStop names.
	Output string
	// leaveAt is when the call leaves its merge to run on outside it, exit
	// 76 (merge.toolLimit); zero: never.
	LeaveAt time.Time
	// acceptRed are the red checks the gate let through (merge.JudgeRed),
	// which devctl's run again accepts.
	acceptRed []string
	// infra are the red checks of the judged run that died on a transient
	// infrastructure failure (merge.InfraReds), which its record names.
	infra []merge.Infra
	// requeued says a hold took the running merge back out of its lane
	// (requeue): its wait for the turn starts again.
	Requeued bool
	// sig hears the signals aimed at the gate from before devctl starts
	// (followSignals), so that none sent while it starts is lost.
	Sig <-chan os.Signal
}

// Reexec replaces this process with the binary installed at its path, with
// env for the new one, and says so; it returns only when the new binary
// does not start, and the call carries on under this one.
func (g *GateRun) Reexec(when string, env ...string) {
	if !g.LeaveAt.IsZero() {
		env = append(env, GateLimitEnv+"="+g.LeaveAt.Format(time.RFC3339Nano))
	}
	GateLine("%s was replaced %s: re-executing it", g.Bin.Path(), when)
	err := g.Bin.Exec(env...)
	GateLine("the new binary does not start (%v): carrying on under %s", err, project.Version())
}

// StaleRefused refuses the call whose save the newer release that wrote the
// state refused and that could not re-execute: nothing ran, the lane is
// untouched, and the installed beekeeper gates the same command again.
func (g *GateRun) StaleRefused(stale *state.StaleWriterError) error {
	load := ""
	if stale.Load != nil {
		load = fmt.Sprintf(", which it does not load (%v)", stale.Load)
	}
	why := fmt.Sprintf("%s: this call runs beekeeper %s, older than the %s that wrote the state%s; nothing ran, the lane is untouched: run the same command again, the installed %s gates it",
		g.Key(), stale.Version, stale.Newer, load, project.Name)
	_ = g.Host.Store().Log(Cmd.Event(g.Me, "merge.refused", "%s", why))
	return GateRefused("%s", why)
}

// stateRefused is a step's answer to a state that does not load or save:
// one a newer release wrote passes on to the wait loop, which re-executes a
// replaced binary or refuses naming both releases (staleRefused); any other
// is refused as it is.
func (g *GateRun) stateRefused(err error) error {
	var stale *state.StaleWriterError
	if errors.As(err, &stale) {
		return stale
	}
	return g.Refuse("the state does not load (%v): fix it, then run the same command again", err)
}

// ResumeMerge carries a running merge on after the re-exec of a replaced
// binary: running names its merge-child and how far its stderr was copied
// (GateRunningEnv). The merge keeps its place, its devctl runs on, and this
// release follows and records it; a merge the state no longer runs is
// followed to its end all the same, its outcome the caller's.
func (g *GateRun) ResumeMerge(running string) error {
	defer Cmd.OutliveCaller()()
	defer g.followSignals()()
	pidText, offsetText, _ := strings.Cut(running, ":")
	pid, err := strconv.Atoi(pidText)
	offset, oerr := strconv.ParseInt(offsetText, 10, 64)
	if err != nil || oerr != nil || pid <= 0 {
		return GateRefused("%s: the re-executed call names no running devctl (%s=%q): check the lane (beekeeper lanes); the merge, if it ran, is recorded by the watch", g.Key(), GateRunningEnv, running)
	}
	if st, err := g.Host.Store().Read(); err == nil {
		if i := g.Mine(st, state.Running); i >= 0 && st.Merges[i].HandCut != "" {
			g.Release = &github.BaseRelease{Base: st.Merges[i].HandCut}
		}
	}
	GateLine("continuing under %s %s: following %s's devctl (pid %d) on", project.Name, project.Version(), g.Key(), pid)
	base, err := g.MergeFiles()
	if err != nil {
		GateLine("%v", err)
		return g.Record(base, ChildRun{Rc: guard.ExitNotFound})
	}
	return g.Follow(base, pid, offset)
}

// Enqueue hands the merge's wait to a run of its own outside the caller
// (LaunchChild), the same gate under --queued: it keeps the merge's place, merges
// when its turn comes and wakes the owner with the outcome (tellOwner), as
// this gate leaves without its heard marker. It returns exit 76, for a
// merge that waited its turn and for one under the budget floor alike. An
// unpinned promotion is refused instead (77): its turn, later, would promote
// a candidate nobody chose.
func (g *GateRun) Enqueue(why string) error {
	if g.Pr == 0 && !g.Pinned {
		return g.Refuse("%s, and only a pinned promotion waits on outside the call: unpinned, its turn would promote whatever candidate is newest by then. Nothing promoted, its place is dropped: run %s release promote %s --version <the candidate you proved> (the newest is %s now)",
			why, merge.Tool, g.Repo, cmp.Or(g.Candidate, "none"))
	}
	// Passive waits do not refresh Seen. Renew the owned place once,
	// before launching the successor: after this PID exits, the ordinary
	// queue TTL must still cover the successor's registration.
	if err := g.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		if i := g.Mine(st, state.Waiting); i >= 0 && st.Merges[i].PID == g.Pid {
			st.Merges[i].Seen = clk.Now().UTC()
		}
		return nil, nil
	}); err != nil {
		var stale *state.StaleWriterError
		if errors.As(err, &stale) {
			return g.StaleRefused(stale)
		}
		return g.stateRefused(err)
	}
	queued := Cmd.NewExitError(ExitGateQueued, "", nil)
	self, err := SelfExe()
	if err == nil {
		argv := []string{self}
		if c := g.Host.ExplicitConfig(); c != "" {
			argv = append(argv, "--config", c)
		}
		if g.Host.As() != "" {
			argv = append(argv, "--as", g.Host.As())
		}
		argv = append(argv, GateCmdName, "--queued", "--wait", g.Host.Cfg().Merge.SeedTTL.String())
		if g.ReviewedHead != "" {
			argv = append(argv, "--reviewed-head", g.ReviewedHead)
		}
		argv = append(append(argv, "--"), g.Argv...)
		_ = os.Setenv(GateFromEnv, strconv.Itoa(g.Pid))
		var pid int
		if pid, err = LaunchChild(ChildSpec{Argv: argv, Command: g.Argv, Owner: g.Me, Config: g.Host.ExplicitConfig()}, Cmd.OwnedBase(g.Host.Store().Dir(), g.Argv, g.Pid)); err == nil {
			GateLine("queued, %s; the merge waits on in a run of its own (pid %d) for up to %s and runs when its turn comes; %s: do not run it again, do not poll",
				why, pid, g.Host.Cfg().Merge.SeedTTL.Duration, g.OutcomeTo())
			_ = g.Host.Store().Log(Cmd.Event(g.Me, "merge.queued", "%s waits on in lane %s in a run of its own (pid %d): %s", g.Key(), g.Lane.Name, pid, why))
			return queued
		}
	}
	kept := g.Host.Cfg().Merge.QueueTTL.Duration
	if g.seeded {
		kept = g.Host.Cfg().Merge.SeedTTL.Duration
	}
	GateLine("queued, %s; it cannot wait on in a run of its own (%v): your place is kept for %s, run the same command again with run_in_background, do not poll",
		why, err, kept)
	return queued
}

// OutcomeTo says where the outcome of a merge that runs on outside the call
// goes: a wake of its owner's session, on the roster or off it, else the log.
func (g *GateRun) OutcomeTo() string {
	if Cmd.WakeID(g.Me) != "" {
		return fmt.Sprintf("its outcome wakes %q", g.Me.Name)
	}
	return "its outcome is logged (beekeeper log --verb merged)"
}

// queuedRun is the files' base of this queued merge's own run, the one
// enqueue launched from the gate g.from; "" for any other call.
func (g *GateRun) queuedRun() string {
	if !g.Queued || g.From == 0 {
		return ""
	}
	return Cmd.OwnedBase(g.Host.Store().Dir(), g.Argv, g.From)
}

// leave ends the call at its tool limit, or as its caller's timeout cut it
// (cut), while the merge's devctl (its merge-child pid) runs on: exit 76, as
// for a queued merge, without the heard marker, so merge-child wakes the
// owner with the outcome (tellOwner), recording the run first (recordLeft);
// the watch's recordGone records one whose merge-child died before that.
func (g *GateRun) leave(pid int, cut bool) error {
	past, why := "its limit", "this call's limit (merge.toolLimit; a desktop Bash call stops at 10 minutes)"
	if cut {
		past, why = "the caller's timeout", "the caller's timeout, which cut the connection, not the merge"
	}
	GateLine("%s's devctl runs past %s: it runs on outside the call (pid %d), %s: do not run it again, do not poll",
		g.Key(), why, pid, g.OutcomeTo())
	_ = g.Host.Store().Log(Cmd.Event(g.Me, "merge.left", "%s: devctl runs on outside the call past %s (pid %d)", g.Key(), past, pid))
	return Cmd.NewExitError(ExitGateQueued, "", nil)
}

// MergeBudgetReserve leaves room for the reads and writes of one gated PR.
const MergeBudgetReserve = 200

// Step joins or refreshes the merge's queue entry and starts devctl when it
// is the merge's turn. It returns what the merge waits for, or "" and the
// run's outcome once devctl ran or the merge was refused.
func (g *GateRun) Step() (string, error) {
	if err := g.ReloadQueueConfig(); err != nil {
		return "", err
	}
	g.Passive = false
	liveWait, claimed := g.waitBehindLive()
	if liveWait != "" && claimed {
		if g.Central {
			if _, err := g.CentralTurn(); err != nil {
				return "", err
			}
		}
		g.Passive = true
		return liveWait, nil
	}
	if liveWait == "" {
		if done, err := g.Takeover(); done || err != nil {
			return "", err
		}
		g.Host.PollToolWindow(g.Ctx, g.Me)
		// Recover an ended devctl before pruning its place as lost. A
		// queued handoff behind a live gate only registers its new PID;
		// its predecessor still owns these checks.
		g.Host.RecordGone(g.Ctx)
		if why, err := g.checkOutside(); err != nil || why != "" {
			return why, err
		}
	}
	if err := g.fatalPublicationBarrier(); err != nil {
		return "", g.Refuse("%v", err)
	}
	var q merge.Lane
	var hold state.Hold
	var held bool
	var dup *state.Merge
	var ahead state.Merge
	behind := false
	dropped := false
	session := ""
	pairRefused := ""
	// A new merge is refused during a hold; one that has its place keeps it
	// and waits until the hold lifts or lets it through.
	placed := g.Placed
	err := g.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		merge.Prune(st, g.Host.Now(), g.Host.Cfg().Merge.QueueTTL.Duration, g.Host.Cfg().Merge.SeedTTL.Duration, proc.Alive)
		if hold, held = merge.BlockingFor(st, g.Host.Now(), g.Me.Name, g.Repo, g.Pr, g.Lane); held && !placed {
			g.drop(st)
			return nil, nil
		}
		for i, m := range st.Merges {
			if m.Repo == g.Repo && m.PR == g.Pr && m.PID != g.Pid && (m.Phase == state.Running ||
				m.Phase == state.Waiting && m.PID != g.From && proc.Alive(m.PID)) {
				dup = &st.Merges[i]
				return nil, nil
			}
		}
		prunePairs(st, g.Host.Now(), g.Host.Cfg().Merge.SeedTTL.Duration)
		if pairRefused = g.pairRefusal(st); pairRefused != "" {
			return nil, nil
		}
		var ev []state.Event
		merge.Retry(st, g.Repo, g.Pr)
		if i := g.Mine(st, state.Waiting); i >= 0 {
			m := &st.Merges[i]
			if strings.HasPrefix(g.Me.Name, "pid ") && m.By.Name != "" && !strings.HasPrefix(m.By.Name, "pid ") {
				g.Me = m.By
			}
			m.PID, m.By, m.Seen, m.Output, m.Run = g.Pid, g.Me, g.Host.Now().UTC(), g.Output, g.queuedRun()
			m.Version = project.Version()
			m.Head = g.mergeTargetHead()
			g.seeded = m.Seeded
			if g.CandidateRead {
				m.Candidate = g.Candidate
			} else {
				g.Candidate = m.Candidate
			}
		} else if g.Placed {
			dropped = true
			return nil, nil
		} else {
			st.Merges = append(st.Merges, state.Merge{Repo: g.Repo, PR: g.Pr, Head: g.mergeTargetHead(), Lane: g.Lane.Name, By: g.Me, PID: g.Pid, Output: g.Output,
				Phase: state.Waiting, Joined: g.Host.Now().UTC(), Seen: g.Host.Now().UTC(), Candidate: g.Candidate})
			detail := fmt.Sprintf("%s in lane %s", g.Key(), g.Lane.Name)
			if g.Pr == 0 {
				detail += ", for " + candidateText(g.Candidate)
			}
			ev = append(ev, Cmd.Event(g.Me, "merge.queued", "%s", detail))
		}
		g.Placed = true
		if held {
			return ev, nil
		}
		q = merge.Queue(st, g.Lane.Name)
		ahead, behind = q.Ahead(g.Repo, g.Pr, merge.Unheld(st, g.Host.Now(), g.Lane, g.Host.Present))
		if st.PairOf(g.Key()) < 0 {
			// A paired merge's slots are decided at its turn (pairTurn).
			session = g.sessionWait(st)
		}
		return ev, nil
	})
	var stale *state.StaleWriterError
	switch {
	case errors.As(err, &stale):
		return "", stale // nothing ran: the gate re-executes a replaced binary, else refuses
	case err != nil:
		return "", GateRefused("the state does not load (%v): fix it, then run the same command again", err)
	case pairRefused != "":
		return "", g.Refuse("%s", pairRefused)
	case dropped:
		return "", g.Refuse("%s was taken out of lane %s (beekeeper lanes drop): nothing ran; run it again only if it is still wanted", g.Key(), g.Lane.Name)
	case held && !placed:
		return "", g.Refuse("%s is held (%s) by %q until %s: %s; park until it lifts (beekeeper agents park --on %s%s \"<what>\"), merge then, do not poll",
			g.Repo, Cmd.HoldTarget(hold), hold.By.Name, Cmd.UntilText(g.Host, hold), hold.Reason, holdParkFlag, hold.Target)
	case dup != nil && dup.Phase == state.Waiting:
		if dup.By.Is(g.Me) {
			GateLine("%s reuses its existing place in lane %s (pid %d); its outcome reaches the owning session", g.Key(), g.Lane.Name, dup.PID)
			return "", ExitCode(ExitGateQueued)
		}
		why := fmt.Sprintf("%s is already queued in lane %s: since %s for %q (pid %d); its outcome reaches %q, do not merge again",
			g.Key(), g.Lane.Name, Cmd.Clock(g.Host.Now(), dup.Joined), dup.By.Name, dup.PID, dup.By.Name)
		_ = g.Host.Store().Log(Cmd.Event(g.Me, "merge.refused", "%s", why))
		return "", GateRefusedWith(ExitGateDuplicate, "%s", why)
	case dup != nil:
		last := Cmd.LastLine(MergeBase(g.Host.Store().Dir(), g.Repo, g.Pr) + ".log")
		if last == "" {
			last = "none yet"
		}
		return "", g.RefuseWith(ExitGateDuplicate, "%s is already merging: started %s by %q (pid %d), last line: %s; its outcome reaches %q, do not merge again",
			g.Key(), Cmd.Clock(g.Host.Now(), dup.Started), dup.By.Name, dup.PID, last, dup.By.Name)
	case held:
		return fmt.Sprintf("held in lane %s (%s) by %q until %s: %s; %s keeps its place and runs once the hold lifts or lets it through",
			g.Lane.Name, Cmd.HoldTarget(hold), hold.By.Name, Cmd.UntilText(g.Host, hold), hold.Reason, g.Key()), nil
	}
	if g.Central {
		if why, err := g.CentralTurn(); err != nil || why != "" {
			return why, err
		}
	}
	if behind {
		g.Passive = liveWait != ""
		if q.Running != nil {
			ahead = *q.Running
		}
		phase := ahead.Phase
		switch {
		case phase != state.Waiting || proc.Alive(ahead.PID):
		case ahead.Outside:
			phase = fmt.Sprintf("settled outside the gate, GitHub reported it not merged at %s", Cmd.Clock(g.Host.Now(), ahead.Checked))
		case ahead.PID != 0:
			phase = fmt.Sprintf("queued, its merge left the gate at %s, its place is kept", Cmd.Clock(g.Host.Now(), ahead.Seen))
		default:
			phase = "queued, its merge has not arrived"
		}
		return fmt.Sprintf("position %d in lane %s behind %s (%q, %s)", q.Position(g.Repo, g.Pr), g.Lane.Name, ahead.Key(), ahead.By.Name, phase), nil
	}
	if q.Running != nil {
		g.Passive = liveWait != ""
		return fmt.Sprintf("next in lane %s behind the running %s (%q, since %s)", g.Lane.Name, q.Running.Key(), q.Running.By.Name,
			Cmd.Clock(g.Host.Now(), q.Running.Started)), nil
	}
	if session != "" {
		return session, nil
	}
	hrs, why, err := g.LaneReady(q)
	if err != nil || why != "" {
		return why, err
	}
	b, err := g.GateBudget()
	if err != nil {
		if merge.Transient(err) {
			return "", g.RefuseWith(ExitGateUnread, "reading the GitHub budget failed in transit (%v), the merge was not judged: run the same command again once GitHub answers", err)
		}
		if graphQLSpentUntil(b, g.Host.Now()) {
			return g.spentWait("the GitHub budget", err)
		}
		return "", g.Refuse("the GitHub budget is unknown (%v): fix that (gh auth status), then run the same command again", err)
	}
	urgent := false
	// One PR has a bounded budget, unlike readers. Keep a conservative
	// reserve for its checks and merge, without admitting other work.
	boundedMerge := g.Pr > 0 && b.Remaining >= MergeBudgetReserve
	if b.Remaining < g.Host.Cfg().GitHub.Floor && !boundedMerge {
		if urgent, why, err = g.urgentTurn(b); err != nil {
			return "", g.stateRefused(err)
		}
		if !urgent {
			g.awaitReset(b.Reset)
			why = cmp.Or(why, fmt.Sprintf("the GitHub budget %d is under the floor %d until the reset at %s", b.Remaining, g.Host.Cfg().GitHub.Floor, Cmd.Clock(g.Host.Now(), b.Reset)))
			if g.Queued {
				return why, nil
			}
			return "", g.Enqueue(why)
		}
	}
	if why, err = g.ReadRelease(); err != nil || why != "" {
		return why, err
	}
	why, err = g.Start(q.SettlingKeys(), hrs)
	if why == "" {
		g.settleUrgent(urgent, b)
	}
	return why, err
}

// ReloadQueueConfig lets waiting gates observe edits without replacing the
// binary. An incomplete edit keeps the last valid settings until the next poll.
func (g *GateRun) ReloadQueueConfig() error {
	path := cmp.Or(g.Host.CfgPath(), g.Host.LoadedCfg())
	if path == "" {
		return nil
	}
	c, err := config.Load(path)
	if err != nil {
		GateLine("queue configuration could not be reloaded; keeping the last valid settings")
		return nil
	}
	lane := c.LaneOf(g.Repo)
	if lane.Name != g.Lane.Name {
		if err := g.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
			if i := g.Mine(st, state.Waiting); i >= 0 {
				st.Merges[i].Lane = lane.Name
			}
			return nil, nil
		}); err != nil {
			return err
		}
	}
	next := *g.Host.Cfg()
	next.GitHub.PublicOnlyOwners = c.GitHub.PublicOnlyOwners
	next.Hooks.Guards = c.Hooks.Guards
	next.Merge.Cap, next.Lanes = c.Merge.Cap, c.Lanes
	g.Host.SetCfg(&next)
	github.SetDevctlSkipVersionCheck(!g.Host.Cfg().Hooks.GuardsEnabled())
	g.Lane = lane
	g.Central = g.Pr != 0 && g.Host.Cfg().CentralLane(lane)
	return nil
}

// checkCandidate refuses a promotion whose turn came, its place running,
// when the newest release candidate is not the one it was queued or pinned
// for: an unpinned promotion would promote another worker's candidate, and a
// pinned one would cut a stable release older than the release candidates
// already out. Its place leaves the lane and its owner decides again.
func (g *GateRun) checkCandidate() error {
	var now string
	err := g.ReadRetrying("the release candidate of "+g.Repo, func() (err error) {
		now, err = PromoteCandidate(g.Ctx, g.Repo)
		return err
	})
	if err == nil && now == g.Candidate {
		return nil
	}
	if err != nil {
		return g.refuseRunning("its turn came, and the release candidate of %s is unknown (%v): nothing promoted; run the same command again", g.Repo, err)
	}
	return g.refuseRunning("%s was queued for %s, the newest is %s now: nothing promoted, its place is dropped; decide again whether to promote it",
		g.Key(), candidateText(g.Candidate), candidateText(now))
}

// refuseRunning refuses the merge whose turn came, its place running: the
// place leaves the lane, which the next merge takes.
func (g *GateRun) refuseRunning(format string, args ...any) error {
	return g.RefuseRunningWith(ExitGateRefused, format, args...)
}

// RefuseRunningWith is refuseRunning with exit code code.
func (g *GateRun) RefuseRunningWith(code int, format string, args ...any) error {
	_ = g.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		if i := g.Mine(st, state.Running); i >= 0 {
			st.Merges = slices.Delete(st.Merges, i, i+1)
		}
		return nil, nil
	})
	return g.RefuseWith(code, format, args...)
}

// candidateText names a release candidate, "" as none.
func candidateText(c string) string {
	if c == "" {
		return "no candidate"
	}
	return "candidate " + c
}

// PromoteCandidate is the release candidate devctl release promote would
// dispatch for repo now (its --dry-run), "" when there is none.
var PromoteCandidate = func(ctx context.Context, repo string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var stderr bytes.Buffer
	c := exec.CommandContext(ctx, merge.Tool, "release", "promote", repo, "--dry-run", "--progress") //nolint:gosec // the merge tool, a checked repository
	c.Env = Cmd.DevctlCommandEnv("")
	c.Stderr = &stderr
	out, err := c.Output()
	if candidate, ok := merge.ParsePromoteCandidate(out); ok {
		return candidate, nil
	}
	if err == nil {
		err = errors.New("no document")
	}
	// CommandContext reports a killed process rather than its deadline. Keep
	// both causes, and use our buffer even when exec.ExitError has no Stderr.
	if cause := ctx.Err(); cause != nil {
		err = fmt.Errorf("%w (%w)", err, cause)
	}
	if tail := cmp.Or(Cmd.LastOf(stderr.String()), merge.PromoteFailure(out)); tail != "" {
		err = fmt.Errorf("%w: %s", err, tail)
	}
	return "", fmt.Errorf("%s release promote %s --dry-run: %w", merge.Tool, repo, err)
}

// outsideCheck is how often a merge waiting in a lane asks GitHub about the
// places before it that wait without their merge.
const outsideCheck = time.Minute

const VerbMerging = "merging"

const VerbPromoting = "promoting"

// checkOutside settles or drops the lane's places that wait without their
// merge and whose pull request merged or closed (checkPlaces). It returns
// what the merge waits for when GitHub does not answer for a place that
// heads the lane, or a StaleWriterError when a newer release wrote the state.
func (g *GateRun) checkOutside() (string, error) {
	err := g.Host.CheckPlaces(g.Ctx, g.Me, g.Host.Now(), g.Lane.Name, g.Repo, g.Pr, outsideCheck)
	if stale := (*state.StaleWriterError)(nil); errors.As(err, &stale) {
		return "", stale
	}
	if err != nil {
		return fmt.Sprintf("lane %s waits for a place settled outside the gate, and GitHub does not answer for it (%v)", g.Lane.Name, err), nil
	}
	return "", nil
}

// ReadRelease reads the pull request's release tree once before merging.
// Commit reads bypass the branch cache because the PR may change workflows.
// A post-merge refresh reads the tree that actually landed.
func (g *GateRun) ReadRelease() (string, error) {
	if g.Release != nil || g.Pr == 0 || !g.RunsDevctl() {
		return "", nil
	}
	what := "whether " + g.Key() + "'s base branch has auto-release"
	if g.Host.Now().Before(g.releaseReset) {
		return g.spentWhy(what, g.releaseReset), nil
	}
	var p github.PullBase
	err := g.ReadRetrying(what, func() (err error) {
		ctx, cancel := context.WithTimeout(g.Ctx, time.Minute)
		defer cancel()
		p, err = PullBase(ctx, g.Repo, g.Pr)
		return err
	})
	var r github.BaseRelease
	if err == nil {
		err = g.ReadRetrying(what, func() (err error) {
			ctx, cancel := context.WithTimeout(g.Ctx, time.Minute)
			defer cancel()
			r, err = ReleaseAt(ctx, g.Repo, p.Base, p.ReleaseRef())
			return err
		})
	}
	switch {
	case github.RateLimited(err):
		return g.spentWait(what, err)
	case err != nil:
		return "", g.refuseKeeping("%s cannot be read (%v): run the same command again", what, err)
	}
	r = p.Of(r)
	g.Release = &r
	return "", nil
}

// spentWait is the wait of a merge whose read GitHub refused under a spent
// limit: it reads the budget afresh and waits in its place for the reset,
// as a merge under the floor does (awaitReset), and reads again then. A
// limit whose reset is unknown refuses, the place kept.
func (g *GateRun) spentWait(what string, err error) (string, error) {
	var b github.Budget
	rerr := g.ReadRetrying("the GitHub budget", func() (rerr error) {
		ctx, cancel := context.WithTimeout(g.Ctx, 30*time.Second)
		defer cancel()
		b, rerr = Cmd.ReadBudget(g.Host, ctx)
		return rerr
	})
	var reset time.Time
	if rerr == nil {
		reset = b.SpentUntil(g.Host.Now())
	}
	if reset.IsZero() {
		return "", g.refuseKeeping("%s cannot be read, GitHub refusing it under a rate limit whose reset is unknown (%v): run the same command again", what, err)
	}
	g.releaseReset = reset
	g.awaitReset(reset)
	why := g.spentWhy(what, reset)
	_ = g.Host.Store().Log(Cmd.Event(g.Me, "merge.spent", "%s: %v", why, err))
	if g.Queued {
		return why, nil
	}
	return "", g.Enqueue(why)
}

// spentWhy is what a merge waits for whose read the spent limit refused.
func (g *GateRun) spentWhy(what string, reset time.Time) string {
	return fmt.Sprintf("%s cannot be read until the GitHub budget's reset at %s, GitHub refusing it under a spent rate limit; %s keeps its place in lane %s and reads it again then",
		what, Cmd.Clock(g.Host.Now(), reset), g.Key(), g.Lane.Name)
}

// RebaseReads is how often the gate reads a pull request whose rebase GitHub
// has not computed yet.
const RebaseReads = 3

// readRebase reads whether the pull request rebases onto its base, again
// while GitHub has not computed it, up to RebaseReads reads.
func (g *GateRun) readRebase() (github.Rebase, error) {
	for try := 1; ; try++ {
		var r github.Rebase
		err := g.ReadRetrying("whether "+g.Key()+" rebases onto its base", func() (err error) {
			ctx, cancel := context.WithTimeout(g.Ctx, time.Minute)
			defer cancel()
			r, err = PullRebase(ctx, g.Repo, g.Pr)
			return err
		})
		if err != nil || r.Known && r.State != UnknownState || try == RebaseReads {
			return r, err
		}
		select {
		case <-g.Ctx.Done():
			return r, g.Ctx.Err()
		case <-clk.After(RebaseRetry):
		}
	}
}

// UnknownState is GitHub's mergeable_state while it computes it.
const UnknownState = "unknown"

// conflictWhy says why GitHub does not rebase-merge r, which conflicts.
func conflictWhy(r github.Rebase) string {
	if r.State == github.DirtyState {
		return "it conflicts with its base (mergeable_state dirty)"
	}
	return "GitHub reports it not rebaseable"
}

// replacementPR is the way out of a rebase merge that cannot be brought onto
// its base.
const replacementPR = "Open a replacement PR (a fresh branch from the base with the change applied again; never merge the base into a branch), then merge that one"

// CheckRebase refuses a rebase merge GitHub reports conflicting with its
// base, or whose head carries a merge commit, before it takes a lane place:
// devctl would refuse the first only at its turn (exit 3, "This branch can't
// be rebased"), and a base merged into the branch hides the conflict from
// the pull request and breaks the line's linear history. GitHub not
// answering refuses; a rebase GitHub has not computed after RebaseReads
// reads queues, devctl judging it at its turn.
func (g *GateRun) CheckRebase() error {
	r, err := g.readRebase()
	switch {
	case err != nil:
		return g.Refuse("whether %s rebases onto its base cannot be read (%v): run the same command again", g.Key(), err)
	case r.Conflicts():
		return g.Refuse("%s cannot be rebased onto its base: %s; a rebase merge fails at its turn, so it takes no lane place. %s",
			g.Key(), conflictWhy(r), replacementPR)
	case !r.Known:
		GateLine("GitHub has not computed whether %s rebases (mergeable_state %s): it queues, devctl judges the rebase at its turn", g.Key(), cmp.Or(r.State, UnknownState))
	}
	merges, err := g.readMergeCommits()
	switch {
	case err != nil:
		return g.Refuse("whether %s carries a merge commit cannot be read (%v): run the same command again", g.Key(), err)
	case len(merges) > 0:
		return g.Refuse("%s carries %s: a rebase merge puts it on the line, so it takes no lane place. %s", g.Key(), mergeCommitsWhy(merges), replacementPR)
	}
	return nil
}

// readMergeCommits reads the pull request's commits with more than one
// parent.
func (g *GateRun) readMergeCommits() ([]string, error) {
	var merges []string
	err := g.ReadRetrying("the commits of "+g.Key(), func() (err error) {
		ctx, cancel := context.WithTimeout(g.Ctx, time.Minute)
		defer cancel()
		merges, err = PullMergeCommits(ctx, g.Repo, g.Pr)
		return err
	})
	return merges, err
}

// mergeCommitsWhy names a pull request's merge commits.
func mergeCommitsWhy(merges []string) string {
	short := make([]string, len(merges))
	for i, m := range merges {
		short[i] = ShortSHA(m)
	}
	return "merge commit " + strings.Join(short, ", ")
}

// ReviewHold reads the pull request's reviews and comments afresh
// (github.ReviewGuard): the reason a reviewer holds its merge, "" when none
// does. devctl pr merge's own guard drops a request for changes once a
// commit or a reply is newer, so the gate reads before devctl runs.
func (g *GateRun) ReviewHold() (string, error) {
	why := ""
	err := g.ReadRetrying("the reviews of "+g.Key(), func() (err error) {
		ctx, cancel := context.WithTimeout(g.Ctx, time.Minute)
		defer cancel()
		authWhy, hasExplicitGo, err := Cmd.MergeAuthority(ctx, g.Host, g.Repo, g.Pr, g.mergeTargetHead())
		if err != nil {
			return err
		}
		if authWhy != "" {
			why = authWhy
			return nil
		}
		why, err = ReviewGuard(ctx, g.Repo, g.Pr)
		if err != nil {
			return err
		}
		if why != "" && hasExplicitGo {
			why = ""
		}
		return nil
	})
	return why, err
}

// headFlag is the flag naming a head: a commit to merge, a branch to open.
const headFlag = "--head"

// mergeTargetHead is the head the gated merge pins: --reviewed-head, else
// the command's --head or --match-head-commit.
func (g *GateRun) mergeTargetHead() string {
	if g.ReviewedHead != "" {
		return g.ReviewedHead
	}
	for i, arg := range g.Argv {
		if (arg == headFlag || arg == "--match-head-commit") && i+1 < len(g.Argv) {
			return g.Argv[i+1]
		}
		if v, ok := strings.CutPrefix(arg, headFlag+"="); ok && v != "" {
			return v
		}
		if v, ok := strings.CutPrefix(arg, "--match-head-commit="); ok && v != "" {
			return v
		}
	}
	return ""
}

// CheckReviews refuses a pull request merge a reviewer holds, through
// refuse: a request for changes that is the reviewer's latest verdict, or
// unanswered feedback newer than the head. Reviews GitHub does not answer
// for refuse too (74 in transit): nothing merges unread.
func (g *GateRun) CheckReviews(refuse func(code int, format string, args ...any) error) error {
	if g.Pr == 0 {
		return nil
	}
	why, err := g.ReviewHold()
	switch {
	case err != nil && (merge.Transient(err) || github.RateLimited(err)):
		return refuse(ExitGateUnread, "reading the reviews of %s failed in transit (%v), nothing merged: run the same command again once GitHub answers", g.Key(), err)
	case err != nil:
		return refuse(ExitGateRefused, "the reviews of %s cannot be read (%v), nothing merged: run the same command again", g.Key(), err)
	case why != "":
		return refuse(ExitGateRefused, "%s waits on its reviewers, nothing merged: %s. Answer the reviewer on the pull request, then run the same command again", g.Key(), why)
	}
	GateLine("reviews: nothing holds %s", g.Key())
	return nil
}

// catchUp rechecks conflicts and merge commits when a rebase merge's turn
// arrives. A head only behind its base is never updated.
func (g *GateRun) catchUp() error {
	if merges, err := g.readMergeCommits(); err != nil {
		GateLine("whether %s carries a merge commit cannot be read (%v): devctl judges it as it stands", g.Key(), err)
	} else if len(merges) > 0 {
		return g.refuseRunning("its turn came, and %s carries %s, which a rebase merge puts on the line; nothing merged. %s", g.Key(), mergeCommitsWhy(merges), replacementPR)
	}
	r, err := g.readRebase()
	switch {
	case err != nil:
		GateLine("whether %s is behind its base cannot be read (%v): devctl judges it as it stands", g.Key(), err)
		return nil
	case r.Conflicts():
		return g.refuseRunning("its turn came, and %s cannot be rebased onto its base: %s; nothing merged. %s", g.Key(), conflictWhy(r), replacementPR)
	case !r.Behind():
		return nil
	}
	// Behind alone is no repair: merge the existing head without restarting CI.
	return nil
}

// ShortSHA is a commit's short form.
func ShortSHA(sha string) string { return sha[:min(len(sha), 9)] }

// handCut is the base branch when no Auto-release run tags a merge into it,
// "" otherwise.
func (g *GateRun) handCut() string {
	if g.Release == nil || g.Release.Auto || g.Release.Plugins || g.Release.ReleasePR {
		return ""
	}
	return g.Release.Base
}

// plugins says whether the base branch's Auto Release tags per plugin
// (github.PluginReleaseWorkflow): devctl awaits no such tag, the gate does
// (awaitPluginTags).
func (g *GateRun) plugins() bool { return g.Release != nil && g.Release.Plugins }

// releasePR says whether the base branch releases through the release-PR
// flow (github.CreateReleaseWorkflow): devctl awaits no release there, the
// gate awaits the tag of a release pull request's merge (awaitReleasePR).
func (g *GateRun) releasePR() bool { return g.Release != nil && g.Release.ReleasePR }

// releasePRFlow names the release-PR flow of the base branch for the gate's
// lines.
func (g *GateRun) releasePRFlow() string {
	return fmt.Sprintf("which releases through the release-PR flow (the bot opens a %s#release#<level> pull request, %s tags its merge)",
		g.Release.Base, filepath.Base(github.CreateReleaseWorkflow))
}

// awaitReleasePR awaits the release a merge into a release-PR branch cuts:
// none for a pull request other than the release pull request, which
// warrants none; the release pull request's tag and its pipeline through
// devctl release wait, within ReleasePRWait, out's release. It returns the
// gate's line on it.
func (g *GateRun) awaitReleasePR(out *merge.Outcome) string {
	v := g.Release.Version
	if v == "" {
		out.NoRelease = true
		return fmt.Sprintf("%s merged into %s, %s: this merge cuts no tag, the release pull request's merge does; lane %s is free",
			g.Key(), g.Release.Base, g.releasePRFlow(), g.Lane.Name)
	}
	GateLine("%s is the release pull request of %s: awaiting its tag pipeline (%s release wait %s %s); lane %s holds until it settles",
		g.Key(), v, merge.Tool, g.Repo, v, g.Lane.Name)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(g.Ctx), ReleasePRWait)
	defer cancel()
	o, reason, ok := merge.ParseReleaseWait(ReleaseWait(ctx, g.Repo, v))
	resume := fmt.Sprintf("`%s release wait %s %s` resumes the wait, do not merge again", merge.Tool, g.Repo, v)
	switch {
	case !ok:
		out.Unconfirmed = true
		return fmt.Sprintf("%s merged, and %s release wait ended without its document: the release %s is unconfirmed; %s", g.Key(), merge.Tool, v, resume)
	case o.ReleaseFailed:
		out.Release, out.ReleaseFailed = o.Release, true
		return fmt.Sprintf("%s merged and its release %s failed (%s): nothing rolls, lane %s settles at once; the fix is the next pull request or a rerun of the failed workflow, do not merge again",
			g.Key(), v, reason, g.Lane.Name)
	case o.Unconfirmed:
		out.Unconfirmed = true
		return fmt.Sprintf("%s merged, and its release %s is unconfirmed (%s): %s", g.Key(), v, reason, resume)
	}
	out.Release = o.Release
	return fmt.Sprintf("%s merged and released %s, its tag pipeline green", g.Key(), o.Release)
}

// awaitPluginTags awaits the tags the base's per-plugin Auto Release cuts
// for its merge commit sha, within pluginTagWait: all cut, they are out's
// release; none due, out warrants no release. It returns the gate's line on
// it.
func (g *GateRun) awaitPluginTags(out *merge.Outcome, sha string) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(g.Ctx), pluginTagWait)
	defer cancel()
	unconfirmed := fmt.Sprintf("its release is unconfirmed: check the Auto Release run of %s, do not merge again", g.Repo)
	tags, err := PluginTags(ctx, g.Repo, sha)
	switch {
	case err != nil:
		return fmt.Sprintf("%s merged, and which plugin tags its Auto Release cuts cannot be read (%v): %s", g.Key(), err, unconfirmed)
	case len(tags) == 0:
		out.NoRelease = true
		return fmt.Sprintf("%s merged and changed no plugin's version in %s: its Auto Release cuts no tag, no release follows, lane %s is free",
			g.Key(), github.Marketplace, g.Lane.Name)
	}
	GateLine("%s merged into %s, whose Auto Release tags per plugin: awaiting %s", g.Key(), g.Release.Base, strings.Join(tags, ", "))
	missing := slices.Clone(tags)
	for {
		missing = slices.DeleteFunc(missing, func(tag string) bool {
			ok, err := TagExists(ctx, g.Repo, tag)
			return err == nil && ok
		})
		if len(missing) == 0 {
			out.Release = strings.Join(tags, " ")
			return fmt.Sprintf("%s merged and released %s, lane %s is free", g.Key(), strings.Join(tags, ", "), g.Lane.Name)
		}
		select {
		case <-ctx.Done():
			return fmt.Sprintf("%s merged, and its Auto Release cut no %s within %s: %s", g.Key(), strings.Join(missing, ", "), pluginTagWait, unconfirmed)
		case <-clk.After(PluginTagPoll):
		}
	}
}

// Key names the merge, owner/repo#n or owner/repo promote.
func (g *GateRun) Key() string { return state.Merge{Repo: g.Repo, PR: g.Pr}.Key() }

// startEvent names the command whose turn came: "merging owner/repo#n" for
// devctl pr merge, "promoting owner/repo" for devctl release promote.
func (g *GateRun) startEvent() state.Event {
	if g.Pr == 0 {
		return Cmd.Event(g.Me, VerbPromoting, "%s in lane %s", g.Repo, g.Lane.Name)
	}
	return Cmd.Event(g.Me, VerbMerging, "%s in lane %s", g.Key(), g.Lane.Name)
}

// NothingDone is the outcome line's "nothing merged", "nothing promoted"
// for a promotion, with devctl's state of it when its document named one.
func NothingDone(pr int, out merge.Outcome) string {
	switch {
	case pr != 0:
		return "nothing merged"
	case out.State != "":
		return "nothing promoted (" + out.State + ")"
	}
	return "nothing promoted"
}

// Mine is the index of this merge's entry in that phase, -1 when none.
func (g *GateRun) Mine(st *state.State, phase string) int {
	return slices.IndexFunc(st.Merges, func(m state.Merge) bool {
		return m.Repo == g.Repo && m.PR == g.Pr && m.Phase == phase && (phase == state.Waiting || m.PID == g.Pid)
	})
}

// drop removes the merge's waiting entry; a seeded place stays, without the
// run that leaves it (its merge-child owes the outcome), and so does another
// session's place a fresh run of the same pull request or promote is refused
// next to (the session a hold admits keeps its turn).
func (g *GateRun) drop(st *state.State) {
	switch i := g.Mine(st, state.Waiting); {
	case i < 0:
	case !g.Placed && !st.Merges[i].By.Is(g.Me):
	case st.Merges[i].Seeded:
		st.Merges[i].Run = ""
	default:
		st.Merges = slices.Delete(st.Merges, i, i+1)
	}
}

// refuseKeeping refuses the merge and keeps its place for merge.seedTTL,
// as a seeded place: the same command, run again, rejoins its lane where it
// was.
func (g *GateRun) refuseKeeping(format string, args ...any) error {
	_ = g.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		if i := g.Mine(st, state.Waiting); i >= 0 {
			st.Merges[i].Seeded, st.Merges[i].Seen = true, g.Host.Now().UTC()
		}
		return nil, nil
	})
	return g.Refuse(format+"; its place in lane %s is kept for %s", append(args, g.Lane.Name, g.Host.Cfg().Merge.SeedTTL.Duration)...)
}

// Refuse removes the merge from its queue, logs the refusal and refuses it.
func (g *GateRun) Refuse(format string, args ...any) error {
	return g.RefuseWith(ExitGateRefused, format, args...)
}

// RefuseWith is refuse with exit code code.
func (g *GateRun) RefuseWith(code int, format string, args ...any) error {
	why := fmt.Sprintf(format, args...)
	g.CentralLeave("refused: " + why)
	_ = g.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		g.drop(st)
		if code == ExitGateRefused {
			g.refusePair(st, why)
			merge.Fail(st, state.Failed{Repo: g.Repo, PR: g.Pr, Head: g.mergeTargetHead(), Lane: g.Lane.Name, By: g.Me, At: g.Host.Now(), Exit: code, Reason: why})
		}
		return []state.Event{Cmd.Event(g.Me, "merge.refused", "%s: %s", g.Key(), why)}, nil
	})
	return GateRefusedWith(code, "%s", why)
}

// LaneReady reads the lane's installation: why is what the lane waits for,
// "" when it is free, which needs each of its settling merges settled. A
// lane without an installation has nothing to roll, but a merge whose
// release is unknown (its run lost, merged without a confirmed release)
// settles it by the settle rule all the same: its release may be cutting
// now, and a promotion behind it would race that. A merge whose release its
// gate confirms (merge.Confirming) lets an overlapping merge start
// (overlaps), which merges only once that release is known (awaitReleases).
// An installation that cannot be read refuses.
func (g *GateRun) LaneReady(q merge.Lane) ([]merge.HelmRelease, string, error) {
	if g.Lane.Installation == "" {
		if why := q.Unsettled(g.Host.Now(), g.Host.Cfg().Merge.Settle.Duration, g.Overlaps(), proc.Alive); why != "" {
			return nil, fmt.Sprintf("next in lane %s, %s", g.Lane.Name, why), nil
		}
		return nil, "", nil
	}
	if st, err := g.Host.Store().Read(); err == nil {
		if h, ok := merge.FixWindow(st, g.Host.Now(), g.Repo, g.Pr, g.Lane); ok {
			GateLine("fix window: %q holds lane %s for %s (%s); it merges without %s Ready or the previous release rolled",
				h.By.Name, g.Lane.Name, g.Key(), h.Reason, g.Lane.Installation)
			_ = g.Host.Store().Update(func(*state.State) ([]state.Event, error) {
				return []state.Event{Cmd.Event(g.Me, "merge.window", "%s in lane %s: %q's fix window waives %s Ready and the previous release rolled (%s)",
					g.Key(), g.Lane.Name, h.By.Name, g.Lane.Installation, h.Reason)}, nil
			})
			return nil, "", nil
		}
	}
	var hrs []merge.HelmRelease
	err := g.ReadRetrying("the HelmReleases of "+g.Lane.Installation, func() (err error) {
		hrs, err = ReadHelmReleases(g.Ctx, g.Lane)
		return err
	})
	if err != nil {
		if cause := g.Host.TeleportCause(g.Ctx); cause != "" {
			return nil, "", g.Refuse("lane %s cannot read the HelmReleases of %s: %s; then run the same command again", g.Lane.Name, g.Lane.Installation, cause)
		}
		if merge.Transient(err) {
			return nil, "", g.Refuse("lane %s cannot read the HelmReleases of %s (%v): run the same command again once it answers", g.Lane.Name, g.Lane.Installation, err)
		}
		return nil, "", g.Refuse("lane %s cannot read the HelmReleases of %s (%v): log in (tsh kube login %s), then run the same command again",
			g.Lane.Name, g.Lane.Installation, err, g.Lane.Installation)
	}
	settling := q.AllSettling
	if len(settling) == 0 {
		settling = []*state.Merge{nil}
	}
	for _, s := range settling {
		ready, why := merge.Ready(g.Lane, hrs, s, g.Host.Now(), g.Host.Cfg().Merge.Settle.Duration)
		switch {
		case ready:
			if why != "" {
				GateLine("lane %s: %s settles without a roll, %s", g.Lane.Name, s.Key(), why)
			}
			continue
		case s != nil && g.Host.Now().Sub(s.Finished) > g.Host.Cfg().Merge.SettleTimeout.Duration:
			return nil, fmt.Sprintf("next in lane %s, stuck since %s: %s; watch says LANE STUCK, the repair is the installation's or lanes clear %s",
				g.Lane.Name, s.Key(), why, g.Lane.Name), nil
		}
		return nil, fmt.Sprintf("next in lane %s, waiting for %s: %s", g.Lane.Name, g.Lane.Installation, why), nil
	}
	return hrs, "", nil
}

// Start makes the merge the lane's running one, when it still is first, its
// lane's settling merges are the ones it checked, the machine runs fewer
// than merge.cap devctl merges and promotes and its party may start one more
// (sessionWait), and runs devctl.
func (g *GateRun) Start(settling string, hrs []merge.HelmRelease) (string, error) {
	if err := g.Host.CheckRepositoryVisibility(g.Ctx, g.Repo); err != nil {
		return "", g.refuseRunning("%s", err)
	}
	if g.RunsDevctl() {
		// devctl refuses to run behind its latest release (exit 7): a merge
		// queued behind a devctl release starts on that release.
		_ = g.Host.InstallTool(g.Ctx, g.Me, "merge.update", "before "+g.Key())
	}
	toolFrom := ""
	openToolWindow := g.ToolMerge()
	if g.ToolMerge() {
		toolFrom = DevctlVersion(g.Ctx)
		// A merged pull request cannot publish a new release on a rerun.
		// Keep an existing window intact, but do not open another one on
		// the installed release. Promotions (PR zero) still need a window.
		// If GitHub cannot answer, retain the conservative window behavior.
		if g.Pr != 0 {
			if p, err := PullState(g.Ctx, g.Repo, g.Pr); err == nil && p.State == github.Merged {
				openToolWindow = false
			}
		}
	}
	why := ""
	var settled []state.Merge
	err := g.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		q := merge.Queue(st, g.Lane.Name)
		i := g.Mine(st, state.Waiting)
		_, behind := q.Ahead(g.Repo, g.Pr, merge.Unheld(st, g.Host.Now(), g.Lane, g.Host.Present))
		w := slices.IndexFunc(st.Holds, func(h state.Hold) bool { return h.Tool != "" && h.ToolMerged })
		switch {
		case i < 0 || behind || q.Running != nil:
			why = "the lane moved on"
			return nil, nil
		case q.SettlingKeys() != settling:
			why = "another merge of the lane just settled"
			return nil, nil
		case g.ToolMerge() && g.Pr != 0 && w >= 0:
			h := st.Holds[w]
			why = fmt.Sprintf("next in lane %s, waiting for %s to report %s, the release of %s (its window lifts then)",
				g.Lane.Name, merge.Tool, h.ToolRelease, ToolKey(h))
			return nil, nil
		}
		var need int
		if why, need = g.PairTurn(st); why != "" {
			return nil, nil
		}
		if why = g.CapWaitFor(st, st.Merges[i], need); why != "" {
			return nil, nil
		}
		if why = g.SessionWaitFor(st, need); why != "" {
			return nil, nil
		}
		g.startPair(st)
		st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool {
			if m.Lane == g.Lane.Name && m.Phase == state.Settling && !merge.Confirming(m, proc.Alive) {
				settled = append(settled, m)
				return true
			}
			return false
		})
		i = g.Mine(st, state.Waiting)
		passed := q.Passed(g.Repo, g.Pr)
		m := &st.Merges[i]
		m.Phase, m.Started, m.Roll, m.Seeded, m.Outside = state.Running, g.Host.Now().UTC(), merge.RollSet(hrs, g.Repo), false, false
		m.HandCut, m.Version = g.handCut(), project.Version()
		m.Finished, m.Exit, m.Reset = time.Time{}, 0, time.Time{}
		ev := []state.Event{g.startEvent()}
		if passed != "" {
			ev[0].Detail += ", ahead of " + passed + " (not arrived)"
		}
		if openToolWindow {
			st.Holds = slices.DeleteFunc(st.Holds, func(h state.Hold) bool { return h.Target == merge.AllMerges })
			h := state.Hold{Target: merge.AllMerges, Except: merge.ToolRepo, By: g.Me, At: g.Host.Now().UTC(), Tool: merge.Tool, ToolFrom: toolFrom,
				ToolPR: g.Pr, Reason: fmt.Sprintf("the devctl release window: %s runs and every new devctl run refuses from its release on until updated, so the other merges are held once its release is coming (its pull request merged, a promotion's at once); the gate runs `devctl version update` once it merged, until the release's binary verifies, and lifts this once `devctl version` reports the release, or at a release candidate, which no update installs", g.Key())}
			st.Holds = append(st.Holds, h)
			ev = append(ev, Cmd.Event(g.Me, "hold.set", "%s except %s until devctl is updated: %s", h.Target, h.Except, h.Reason))
		}
		return ev, nil
	})
	if err != nil {
		return "", g.stateRefused(err)
	}
	if why != "" {
		return why, nil
	}
	g.Host.LeaveCentral(g.Ctx, settled, "rolled, HelmReleases of "+g.Lane.Installation+" Ready")
	if g.Pr == 0 {
		if err := g.checkCandidate(); err != nil {
			return "", err
		}
	}
	if g.Central {
		if why, err := g.CentralStart(); err != nil || why != "" {
			return why, err
		}
	}
	if g.Pr != 0 && merge.Rebase(g.Argv) {
		if err := g.catchUp(); err != nil {
			return "", err
		}
	}
	// Feedback that landed while the merge waited holds it too.
	if err := g.CheckReviews(g.RefuseRunningWith); err != nil {
		return "", err
	}
	err = g.RunMerge()
	if p := (*PreemptedError)(nil); errors.As(err, &p) {
		return p.Why, nil
	}
	return "", err
}

// CapWait is what the merge me, its lane's next, waits for under merge.cap,
// "" when it may start: the machine's devctl merges and promotes count, the
// waits never, except in the merge's reset window (merge.resetFirst): only
// the running merges count, and the older merges queued for the reset that
// are free to start take their slots first.
func (g *GateRun) CapWait(st *state.State, me state.Merge) string { return g.CapWaitFor(st, me, 1) }

// CapWaitFor is capWait for a start that takes need slots: two for the
// first merge of a pair, which starts its sibling too (pairTurn), none for
// that sibling.
func (g *GateRun) CapWaitFor(st *state.State, me state.Merge, need int) string {
	if need <= 0 {
		return ""
	}
	most := g.Host.Cfg().Merge.Cap
	pair := ""
	if need > 1 {
		pair = fmt.Sprintf(", it and the merge it lands with need %d slots", need)
	}
	window := g.Host.Cfg().Merge.ResetFirst.Duration
	if !merge.InResetWindow(me, g.Host.Now(), window) {
		if n := capRuns(st); n+need > most {
			return fmt.Sprintf("%d merges and promotes run machine-wide (cap %d)%s, not a lane problem: %s is next in lane %s and starts when one ends",
				n, most, pair, g.Key(), g.Lane.Name)
		}
		return ""
	}
	ahead := merge.ResetAhead(st, me, g.Host.Now(), window, g.Host.Present, proc.Alive)
	running := len(runningMerges(st))
	if running+len(ahead)+need <= most {
		return ""
	}
	if len(ahead) > 0 {
		return fmt.Sprintf("%d merges run (cap %d) and %s, queued for the reset before it, goes first: %s is next in lane %s",
			running, most, merge.Keys(ahead), g.Key(), g.Lane.Name)
	}
	return fmt.Sprintf("%d merges run (cap %d): %s, queued for the reset, is next in lane %s and starts when one ends",
		running, most, g.Key(), g.Lane.Name)
}

// awaitReset records on the merge's place the budget reset it waits for
// under the floor: from then it goes before the waits (merge.ResetFirst).
func (g *GateRun) awaitReset(reset time.Time) {
	_ = g.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		if i := g.Mine(st, state.Waiting); i >= 0 {
			st.Merges[i].Reset = reset.UTC()
		}
		return nil, nil
	})
}

// sessionWait is what the merge, its lane's next, waits for under
// merge.sessionCap, "" when its party may start one more: each running
// devctl polls GitHub, so one party's queued merges run a few at a time and
// the rest wait in their lanes without reading GitHub, starting in the order
// they queued.
func (g *GateRun) sessionWait(st *state.State) string { return g.SessionWaitFor(st, 1) }

// SessionWaitFor is sessionWait for a start that takes need of the party's
// slots (capWaitFor): a pair's first keeps one free for its sibling.
func (g *GateRun) SessionWaitFor(st *state.State, need int) string {
	i := g.Mine(st, state.Waiting)
	if i < 0 || need <= 0 {
		return ""
	}
	most := g.Host.Cfg().Merge.SessionCap
	present := g.Host.Present
	if p := st.PairOf(g.Key()); need > 1 && most > 0 && p >= 0 {
		// The sibling takes the slot kept free, not one before this merge.
		most = max(most-need+1, 1)
		sib := st.Pairs[p].Sibling(g.Key())
		present = func(m state.Merge) bool { return m.Key() != sib && g.Host.Present(m) }
	}
	running, first, ok := merge.SessionTurn(st, st.Merges[i], most, present, proc.Alive)
	switch {
	case ok:
		return ""
	case first != nil:
		return fmt.Sprintf("next in lane %s, %s of %q queued before it takes the free one of its %d merge slots (merge.sessionCap): it waits without reading GitHub",
			g.Lane.Name, first.Key(), g.Me.Name, most)
	}
	keys := make([]string, len(running))
	for j, m := range running {
		keys[j] = m.Key()
	}
	return fmt.Sprintf("next in lane %s, %q runs %d merges (%s), merge.sessionCap %d: it waits without reading GitHub until one ends",
		g.Lane.Name, g.Me.Name, len(running), strings.Join(keys, ", "), most)
}

// unstart puts the merge back to waiting in its local lane when its central
// lane did not let it start: it keeps its place in both.
func (g *GateRun) unstart() {
	_ = g.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		if i := g.Mine(st, state.Running); i >= 0 {
			st.Merges[i].Phase, st.Merges[i].Started, st.Merges[i].Roll = state.Waiting, time.Time{}, nil
		}
		return nil, nil
	})
}

// RunMerge runs devctl once, detached from its caller (LaunchChild), its
// document and exit code unchanged, and follows it to its end (follow). A
// run that does not start is recorded as one without a document.
func (g *GateRun) RunMerge() error {
	defer Cmd.OutliveCaller()()
	defer g.followSignals()()
	if !g.RunsDevctl() {
		GateLine("devctl serves the repositories of %s only (merge.devctlOwners): %s#%d takes the %s as the gh login, green first, no release wait",
			strings.Join(g.Host.Cfg().Merge.DevctlOwners, ", "), g.Repo, g.Pr, github.SquashRoute)
	}
	switch b := g.handCut(); {
	case b != "":
		GateLine("%s merges into %s, which no Auto-release run tags: devctl ends at the merge (--no-release-wait), awaits no release and frees lane %s then",
			g.Key(), b, g.Lane.Name)
	case g.plugins():
		GateLine("%s merges into %s, whose Auto Release tags per plugin (%s): devctl ends at the merge (--no-release-wait), the gate awaits the tags of the plugins whose version it changes",
			g.Key(), g.Release.Base, github.PluginReleaseWorkflow)
	case g.releasePR() && g.Release.Version != "":
		GateLine("%s merges into %s, %s: it is the release pull request of %s, devctl ends at the merge (--no-release-wait) and the gate awaits the tag pipeline of %s",
			g.Key(), g.Release.Base, g.releasePRFlow(), g.Release.Version, g.Release.Version)
	case g.releasePR():
		GateLine("%s merges into %s, %s: devctl ends at the merge (--no-release-wait), this merge awaits no release and frees lane %s then",
			g.Key(), g.Release.Base, g.releasePRFlow(), g.Lane.Name)
	}
	if err := g.AwaitReleases(); err != nil {
		return err
	}
	if err := g.fatalPublicationBarrier(); err != nil {
		return g.refuseRunning("%v", err)
	}
	base, err := g.MergeFiles()
	if err != nil {
		GateLine("%v", err)
		return g.Record(base, ChildRun{Rc: guard.ExitNotFound})
	}
	pid, rc := g.launch(base)
	if rc != 0 {
		return g.Record(base, ChildRun{Rc: rc})
	}
	return g.Follow(base, pid, 0)
}

// MergeArgv is the command the merge's merge-child runs: the gated devctl
// command bounded by the gate's CI timeout, with --no-release-wait into a
// hand-cut base, or the plain squash merge for a repository devctl does not
// serve.
func (g *GateRun) MergeArgv() ([]string, error) {
	if err := g.CheckMergeRoute(); err != nil {
		return nil, err
	}
	if g.ReviewedHead != "" {
		self, err := SelfExe()
		if err != nil {
			return nil, err
		}
		argv := []string{self, squashMergeCmd, g.Repo, fmt.Sprint(g.Pr), headFlag, g.ReviewedHead}
		if g.Release != nil && g.Release.Auto {
			argv = append(argv, "--release-wait")
		}
		return argv, nil
	}
	argv := g.Argv
	if !g.RunsDevctl() {
		self, err := SelfExe()
		if err != nil {
			return nil, err
		}
		argv = SquashArgv(self, g.Repo, g.Pr, g.Argv)
	}
	if g.Pr != 0 && g.RunsDevctl() {
		argv = merge.CITimeout(argv, g.Host.Cfg().Merge.TimeoutFor(g.Repo))
		argv = append(slices.Clone(argv), "--progress")
	}
	if g.handCut() != "" || g.plugins() || g.releasePR() || g.Overlaps() {
		argv = merge.NoReleaseWait(argv)
	}
	return merge.AcceptRed(argv, g.acceptRed), nil
}

// launch starts the merge's merge-child (LaunchChild) and records it as the
// merge's child; rc is the exit to record when it does not start.
func (g *GateRun) launch(base string) (pid, rc int) {
	argv, err := g.MergeArgv()
	if err == nil {
		pid, err = LaunchChild(ChildSpec{Argv: argv, Owner: g.Me, Config: g.Host.ExplicitConfig(), HandCut: g.handCut(), BusyBudget: g.busyBudget(), BusyPauses: BusyPauses}, base)
	}
	if err != nil {
		GateLine("devctl does not start: %v", err)
		RemoveMergeFiles(base)
		return 0, guard.ExitNotFound
	}
	g.Started(pid)
	return pid, 0
}

// busyBudget is how long the merge's merge-child runs devctl again after a
// run ends on a busy base (merge.BusyBase): merge.busyBudget for a pull
// request's merge through devctl with --update-branch, which keeps its lane
// place meanwhile, so no other gated merge of the repository moves its base;
// zero, devctl runs once, for any other.
func (g *GateRun) busyBudget() time.Duration {
	if g.Pr == 0 || !g.RunsDevctl() || !merge.UpdatesBranch(g.Argv) {
		return 0
	}
	return max(g.Host.Cfg().Merge.BusyBudget.Duration, 0)
}

// BusyPauses are the pauses before a busy base's next devctl run; a seam
// for the tests.
var BusyPauses = merge.BusyPauses

// ServerRetries are the pauses before the gate sends a merge again whose
// merge call GitHub answered with a 5xx (merge.ServerError), one per retry.
var ServerRetries = []time.Duration{10 * time.Second, 30 * time.Second, time.Minute}

// retryServerError runs the merge again while its last run ended on a 5xx
// of its merge call and GitHub reports the pull request still open: devctl
// sends the merge once, a gateway's 502 is GitHub's, and the expected head
// keeps a second call from merging anything but the judged head. A pull
// request GitHub reports otherwise, or does not answer for, is the record's
// to judge, its document set aside. It returns the last run.
func (g *GateRun) retryServerError(base string, pid int, run ChildRun) (ChildRun, int) {
	for try, pause := range ServerRetries {
		if run.Replaced || run.Left || !merge.ServerError(run.Doc) {
			return run, pid
		}
		p, err := PullState(context.WithoutCancel(g.Ctx), g.Repo, g.Pr)
		if err != nil || p.State != github.Open {
			run = run.emit()
			run.Doc = nil
			return run, pid
		}
		GateLine("GitHub answered %s's merge call with a server error (exit %d): sending it again in %s, retry %d of %d",
			g.Key(), run.Rc, pause, try+1, len(ServerRetries))
		_ = g.Host.Store().Log(Cmd.Event(g.Me, "merge.retry", "%s: a 5xx on the merge call, retry %d of %d", g.Key(), try+1, len(ServerRetries)))
		clk.Sleep(pause) // the held document goes: a retry's replaces it
		var rc int
		if pid, rc = g.launch(base); rc != 0 {
			return ChildRun{Rc: rc}, pid
		}
		run = FollowChild(base, pid, 0, g.Bin, g.retried(try+1), g.LeaveAt, false, g.Sig)
	}
	return run, pid
}

// retried is FollowChild's hold for a run after try retries: a document of
// a 5xx on the merge call while a retry is left, and a red one while the
// gate may judge it (acceptRedRun).
func (g *GateRun) retried(try int) func([]byte) bool {
	return func(doc []byte) bool {
		return try < len(ServerRetries) && merge.ServerError(doc) || g.judgesRed() && merge.Red(doc)
	}
}

// judgesRed says whether a red run of this merge is the gate's to judge: a
// pull request's merge through devctl that has accepted no red check yet.
func (g *GateRun) judgesRed() bool { return g.Pr != 0 && g.RunsDevctl() && len(g.acceptRed) == 0 }

// acceptRedRun runs the merge again with devctl's --accept-red for each red
// check the gate lets through, when every red check of the run is let
// through (merge.JudgeRed): not required by the base, no security scan, and
// red on the base branch's head too. A run judged otherwise ends as it did,
// and so does one whose installed devctl lacks the flag; the gate's line
// names the judgement either way. It returns the last run.
func (g *GateRun) acceptRedRun(base string, pid int, run ChildRun) (ChildRun, int) {
	if !g.judgesRed() || run.Replaced || run.Left || !merge.Red(run.Doc) {
		return run, pid
	}
	ctx := context.WithoutCancel(g.Ctx)
	j := g.judgeRed(ctx, run.Doc)
	switch {
	case !j.LetThrough():
		GateLine("%s is red: %s", g.Key(), j)
		return run, pid
	case !DevctlAcceptsRed(ctx):
		GateLine("%s is red only on checks the gate lets through (%s), but the installed devctl has no %s: the merge waits for a devctl release that has it, then the same command again",
			g.Key(), j, merge.AcceptRedFlag)
		return run, pid
	}
	GateLine("%s is red only on checks the gate lets through (%s): devctl runs again with %s", g.Key(), j, merge.AcceptRedFlag)
	_ = g.Host.Store().Log(Cmd.Event(g.Me, "merge.acceptred", "%s: %s", g.Key(), j))
	g.acceptRed = j.Names()
	var rc int
	if pid, rc = g.launch(base); rc != 0 {
		return ChildRun{Rc: rc}, pid
	}
	return g.retryServerError(base, pid, FollowChild(base, pid, 0, g.Bin, g.retried(0), g.LeaveAt, false, g.Sig))
}

// judgeRed judges a red document against the base branch it names; a base
// GitHub does not answer for lets nothing through.
func (g *GateRun) judgeRed(ctx context.Context, doc []byte) merge.RedJudgement {
	g.infra = InfraReds(ctx, doc)
	ref := merge.BaseRef(doc)
	if ref == "" {
		return merge.RedJudgement{Refused: []string{"devctl's document names no base branch"}}
	}
	b, err := BaseChecks(ctx, g.Repo, ref)
	if err != nil {
		return merge.RedJudgement{Refused: []string{fmt.Sprintf("the checks of %s do not read: %v", ref, err)}}
	}
	return merge.JudgeRed(doc, b, g.infra)
}

// followRun follows the merge's devctl from offset to the end of its last
// run: sent again on a 5xx of its merge call (retryServerError), and with
// the red checks the gate lets through accepted (acceptRedRun).
func (g *GateRun) followRun(base string, pid int, offset int64, bin Binary) (ChildRun, int) {
	run, pid := g.retryServerError(base, pid, FollowChild(base, pid, offset, bin, g.retried(0), g.LeaveAt, false, g.Sig))
	return g.acceptRedRun(base, pid, run)
}

// followSignals has g.sig hear SIGINT, SIGTERM and SIGHUP from now on, for
// every FollowChild of the merge's devctl: a signal sent while devctl
// starts waits there for the follow; the returned func stops it.
func (g *GateRun) followSignals() func() {
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	g.Sig = sig
	return func() { signal.Stop(sig); g.Sig = nil }
}

// Follow copies the merge's devctl (its merge-child pid) stderr on from
// offset to its end and records the run (record). A binary replaced
// meanwhile re-executes the installed one first (reexec), which follows the
// same devctl on from there (resumeMerge), so the installed release records
// the outcome; one that does not start is followed on under this one.
func (g *GateRun) Follow(base string, pid int, offset int64) error {
	stop := g.watchPreempt()
	run, pid := g.followRun(base, pid, offset, g.Bin)
	if run.Replaced {
		stop()
		g.Reexec("while "+g.Key()+"'s devctl ran", fmt.Sprintf("%s=%d:%d", GateRunningEnv, pid, run.Offset))
		stop = g.watchPreempt()
		run, _ = g.followRun(base, pid, run.Offset, nil)
	}
	if h, preempted := stop(); preempted && !run.Left {
		if err := g.Requeue(base, h); err != nil {
			return err
		}
	}
	run = run.emit()
	if run.Left {
		return g.leave(pid, run.cut)
	}
	if !g.wakesOnFailure(run) {
		defer Cmd.HandOver(base, run, g.Cli)
	}
	return g.Record(base, run)
}

// wakesOnFailure says whether a run's outcome goes to its owner as a wake
// even while the caller listens: a merge whose document says nothing merged,
// which the owner acts on, also when its call ran in the background of a turn
// that ended. merge-child wakes it (tellOwner) as no heard marker is left; a
// queued merge's own run leaves that to the run that waits for it, which wakes
// the owner once.
func (g *GateRun) wakesOnFailure(run ChildRun) bool {
	if g.Pr == 0 || g.Queued {
		return false
	}
	out, ok := merge.ParseDocument(run.Doc)
	return ok && !out.Merged
}

// failureWake is the gate line's word on the wake a merge that merged
// nothing sends its owner, "" when it sends none.
func (g *GateRun) failureWake(run ChildRun) string {
	if !g.wakesOnFailure(run) || g.Me.Session == "" && g.Me.HostSession == "" {
		return ""
	}
	return fmt.Sprintf(" (the outcome wakes %q too)", g.Me.Name)
}

// Record records the run's outcome: a merge settles its lane, one that
// warranted no release or whose lane has no installation to roll leaves it
// once its release is known, and so does one with nothing merged. A run without
// its document or ended by a signal is GitHub's to judge: merged, its
// release is recovered with release wait; unanswered, the lane settles by
// the settle rule as for a lost merge. The run's files go with the record; a save the newer
// release that wrote the state refuses leaves them, document and exit code,
// for the watch, which records the run from them at its next poll.
func (g *GateRun) Record(base string, run ChildRun) error {
	doc, rc, output := run.Doc, run.Rc, run.kept
	note := ""
	if g.ReviewedHead != "" {
		note = ", reviewed merge"
	} else if !g.RunsDevctl() {
		note = ", " + github.SquashRoute
	}
	if output != "" {
		note += ", output in " + output
	}
	r := RunOutcome{Rc: rc, Reason: Cmd.WithInfra(Cmd.RunFailure(doc, Cmd.LastLine(base+".log"), g.handCut()), g.infra)}
	var ok bool
	r.Out, ok, rc = ParseRunOutcome(g.Pr, doc, rc)
	r.Rc = rc
	if merge.NeedsJudging(ok, rc) {
		r.Out, r.Unanswered = JudgeRun(g.Ctx, g.Repo, g.Pr, judgeTries)
	}
	// Keep the running place until recovery finishes, so an unknown release
	// cannot enter settling while this gate can still identify it. A gate
	// that was itself stopped ends now: lanes settle --release names it later.
	if r.Out.Merged && r.Out.Unconfirmed && r.Unanswered == nil && g.Pr != 0 && g.handCut() == "" && !run.stopped {
		GateLine("%s merged per GitHub; recovering its release (%s release wait %s --pr %d)", g.Key(), merge.Tool, g.Repo, g.Pr)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(g.Ctx), ReleasePRWait)
		recovered, _, found := merge.ParseReleaseWait(ReleaseWait(ctx, g.Repo, "--pr", strconv.Itoa(g.Pr)))
		cancel()
		if found && !recovered.Unconfirmed && (recovered.Release != "" || recovered.NoRelease || recovered.ReleaseFailed) {
			recovered.MergedAt = r.Out.MergedAt
			r.Out = recovered
		}
	}
	releaseLine := ""
	// Refresh after merging: another merge may have changed the workflows
	// since the preflight read. Never free a lane on an unread release flow.
	if r.Out.Merged && g.Pr != 0 && g.Release != nil {
		baseBranch := g.Release.Base
		wasAuto := g.Release.Auto
		var fresh github.BaseRelease
		err := g.ReadRetrying("the merged release workflow", func() (err error) {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(g.Ctx), time.Minute)
			defer cancel()
			if sha := merge.MergeCommit(doc); sha != "" {
				fresh, err = ReleaseAt(ctx, g.Repo, baseBranch, sha)
				fresh.Version = g.Release.Version
			} else {
				var p github.PullBase
				p, err = PullBase(ctx, g.Repo, g.Pr)
				if err == nil && p.MergeOID == "" {
					err = fmt.Errorf("no merge commit reported")
				}
				if err == nil {
					fresh, err = ReleaseAt(ctx, g.Repo, p.Base, p.MergeOID)
					fresh = p.Of(fresh)
				}
			}
			return err
		})
		if err == nil {
			g.Release = &fresh
		}
		if err != nil {
			r.Out.Unconfirmed, r.Out.NoRelease = true, false
			r.Out.Release, rc = "", DevctlUnconfirmed
			g.Release = &github.BaseRelease{Base: baseBranch, Auto: true}
			releaseLine = fmt.Sprintf("%s merged; its release workflow cannot be read (%v): release unconfirmed, lane %s remains held", g.Key(), err, g.Lane.Name)
		} else if g.Release.Auto && r.Out.Release == "" && (!r.Out.NoRelease || !wasAuto) && !g.Overlaps() {
			r.Out.NoRelease = false
			ctx, cancel := context.WithTimeout(context.WithoutCancel(g.Ctx), ReleasePRWait)
			waited, _, ok := merge.ParseReleaseWait(ReleaseWait(ctx, g.Repo, "--pr", strconv.Itoa(g.Pr)))
			cancel()
			if ok {
				r.Out.Release, r.Out.NoRelease = waited.Release, waited.NoRelease
				r.Out.ReleaseFailed, r.Out.Unconfirmed = waited.ReleaseFailed, waited.Unconfirmed
			} else {
				r.Out.Unconfirmed = true
			}
			if r.Out.Unconfirmed {
				rc = DevctlUnconfirmed
			}
			if r.Out.ReleaseFailed {
				rc = DevctlReleaseFailed
			}
			releaseLine = fmt.Sprintf("%s merged into %s with Auto-release: release %s", g.Key(), g.Release.Base, cmp.Or(r.Out.Release, "unconfirmed"))
		} else if g.handCut() != "" {
			r.Out.NoRelease = true
		} else if g.Release.Auto && !wasAuto {
			r.Out.NoRelease = false
		}
		if err == nil && g.Release.Auto && r.Out.Release != "" && releaseLine == "" {
			releaseLine = fmt.Sprintf("%s merged into %s with Auto-release: release %s", g.Key(), g.Release.Base, r.Out.Release)
		}
	}
	switch sha := merge.MergeCommit(doc); {
	case g.plugins() && r.Out.Merged && sha != "":
		releaseLine = g.awaitPluginTags(&r.Out, sha)
	case g.releasePR() && r.Out.Merged && r.Unanswered == nil:
		releaseLine = g.awaitReleasePR(&r.Out)
	}
	// devctl ended at the merge (overlaps): a clean merge's release is
	// confirmed once its lane place is releasing; a run judged by GitHub
	// settles by the settle rule, as without the overlap.
	confirm := g.Overlaps() && rc == 0 && r.Out.Merged && r.Unanswered == nil && !r.Out.Unconfirmed && !r.Out.NoRelease && r.Out.Release == ""
	settles := false
	err := g.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		i := g.Mine(st, state.Running)
		if i < 0 {
			confirm = false
			return nil, nil
		}
		r.Confirming = confirm
		st.Merges[i].HandCut = g.handCut()
		ev := Cmd.RecordRun(st, i, g.Lane, g.Me, r, clk.Now().UTC(), note)
		j := slices.IndexFunc(st.Merges, func(m state.Merge) bool {
			return m.Repo == g.Repo && m.PR == g.Pr && m.Phase == state.Settling
		})
		settles = j >= 0
		if confirm = confirm && settles; confirm {
			st.Merges[j].Releasing, st.Merges[j].PID = true, os.Getpid()
		}
		return g.Host.MergeEvents(ev)
	})
	if confirm && err == nil {
		releaseLine, rc = g.ConfirmRelease(&r.Out, rc)
	}
	var stale *state.StaleWriterError
	if errors.As(err, &stale) {
		GateLine("%s's outcome is not recorded by this call, which runs beekeeper %s, older than the %s that wrote the state: its document and exit code stay in %s for the watch of the installed %s, which records the run at its next poll",
			g.Key(), stale.Version, stale.Newer, filepath.Dir(base), project.Name)
	} else if err != nil {
		return err
	} else {
		RemoveMergeFiles(base)
	}
	if g.Central {
		g.CentralRecord(settles, fmt.Sprintf("devctl exit %d, nothing to roll", rc))
	}
	out, unanswered := r.Out, r.Unanswered
	if g.ToolMerge() && out.Merged {
		// The release is out: install it now rather than on the watch's tick,
		// also when its window was lifted by hand, then lift the window.
		ctx := context.WithoutCancel(g.Ctx)
		_ = g.Host.InstallTool(ctx, g.Me, "hold.update", "after "+g.Key())
		g.Host.CloseToolWindow(ctx, g.Me)
	}
	switch {
	case unanswered != nil:
		GateLine("devctl ended with exit %d without its document and GitHub does not answer (%v): whether %s merged is unknown, lane %s settles by the settle rule; check the pull request, do not rerun blindly",
			rc, unanswered, g.Key(), g.Lane.Name)
	case releaseLine != "":
		GateLine("%s", releaseLine)
	case out.Merged && g.handCut() != "":
		GateLine("%s merged into %s, which no Auto-release run tags: no release awaited, lane %s is free; a tag of %s is cut by hand",
			g.Key(), g.handCut(), g.Lane.Name, g.handCut())
	case out.ReleaseFailed:
		GateLine("%s merged and its release failed (exit %d): nothing rolls, lane %s settles at once; the fix is the next pull request or a rerun of the failed workflow, do not merge again",
			g.Key(), rc, g.Lane.Name)
	case out.Unconfirmed:
		GateLine("devctl ended with exit %d before its document, and GitHub reports %s#%d merged: its release is unconfirmed, recover it with `devctl release wait %s --pr %d`, then name it with `beekeeper lanes settle %s %d --release <version>`; do not merge again",
			rc, g.Repo, g.Pr, g.Repo, g.Pr, g.Repo, g.Pr)
	case !out.Merged && (rc == devctlUsage || rc == DevctlAuth):
		GateLine("%s (exit %d, a tooling fault): the same command fails the same way until what the reason names is fixed (output in %s); %s left lane %s",
			NothingDone(g.Pr, out), rc, output, g.Key(), g.Lane.Name)
	case !out.Merged:
		GateLine("%s (%s); %s left lane %s%s: act on the reason, then run the same command again, it joins the lane anew",
			NothingDone(g.Pr, out), ExitText(rc, r.Reason), g.Key(), g.Lane.Name, g.failureWake(run))
	case g.Pr == 0:
		GateLine("promoted %s: release %s dispatched", g.Repo, out.Release)
	}
	return ExitCode(rc)
}

// Started records the merge's devctl, which runs on when the gate is gone.
func (g *GateRun) Started(pid int) {
	_ = g.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		if i := g.Mine(st, state.Running); i >= 0 {
			st.Merges[i].Child = pid
		}
		return nil, nil
	})
}

// parseOutcome reads the document of the gated command: a merge's, or a
// promotion's for pr 0.
func parseOutcome(pr int, doc []byte) (merge.Outcome, bool) {
	if pr == 0 {
		return merge.ParsePromoteDocument(doc)
	}
	return merge.ParseDocument(doc)
}

// ParseRunOutcome treats promote's usage exit as a certain refusal, even
// without a JSON document. No GitHub mutation can have happened.
func ParseRunOutcome(pr int, doc []byte, rc int) (merge.Outcome, bool, int) {
	if pr == 0 && rc == devctlUsage {
		return merge.Outcome{}, true, 3
	}
	out, ok := parseOutcome(pr, doc)
	return out, ok, rc
}

// ToolMerge says whether the gate merges or promotes the merge tool's own
// repository, which opens the tool-release window.
func (g *GateRun) ToolMerge() bool { return strings.EqualFold(g.Repo, merge.ToolRepo) }

// RunsDevctl says whether the gated command runs devctl: a promotion, or a
// merge of a repository devctl serves; any other merge takes the plain
// squash merge.
func (g *GateRun) RunsDevctl() bool { return g.Pr == 0 || g.Host.Cfg().Merge.DevctlServes(g.Repo) }

// CheckMergeRoute refuses branch updates on every merge route, before
// queuing and again before launch for runs resumed from an earlier version.
func (g *GateRun) CheckMergeRoute() error {
	if merge.UpdatesBranch(g.Argv) {
		return GateRefused("--update-branch is disabled: merge the green PR head after merge-result validation; the gate cannot update the branch; nothing merged")
	}
	return nil
}

// judgeTries is how often the gate asks GitHub about a run without its
// document, JudgeWait the pause between the tries.
const judgeTries = 3

var JudgeWait = 10 * time.Second

// JudgeRun asks GitHub, up to tries times, whether repo#pr merged, as its
// run left no document to say it; the caller's context may be gone, the
// gate's is not.
func JudgeRun(ctx context.Context, repo string, pr, tries int) (merge.Outcome, error) {
	if pr == 0 {
		return merge.Outcome{}, errors.New("a promotion has no pull request to ask about")
	}
	ctx = context.WithoutCancel(ctx)
	var err error
	for try := range tries {
		if try > 0 {
			clk.Sleep(JudgeWait)
		}
		var p github.Pull
		if p, err = PullState(ctx, repo, pr); err == nil {
			return merge.Judged(p), nil
		}
	}
	return merge.Outcome{}, err
}

// ExitText is "exit <rc>", with the run's reason when it gave one.
func ExitText(rc int, reason string) string {
	if reason == "" {
		return fmt.Sprintf("exit %d", rc)
	}
	return fmt.Sprintf("exit %d: %s", rc, reason)
}

// RunOutcome is how one merge's devctl run ended: its outcome, its exit
// code, its reason (runReason), and GitHub's error when nothing could judge
// it.
type RunOutcome struct {
	Out        merge.Outcome
	Rc         int
	Reason     string
	Unanswered error
	// confirming marks a merge whose release its gate confirms after the
	// merge (GateRun.overlaps): the verdict follows as merge.release.
	Confirming bool
}

// ToolKey names the merge or promotion that opened the tool-release window h.
func ToolKey(h state.Hold) string { return state.Merge{Repo: merge.ToolRepo, PR: h.ToolPR}.Key() }

// GateBudget is the last budget reading when it is younger than
// merge.budgetFresh and its reset has not come, else a fresh one (a
// conditional request, free on 304): a merge queued for the reset reads
// the new window at once. Neither needs the state lock: the last reading
// is read without it (a write replaces the state in one rename), and a
// fresh reading that only lost its save to a busy lock stands (#3411).
func (g *GateRun) GateBudget() (state.Budget, error) {
	st, err := g.Host.Store().Peek()
	if err != nil {
		return state.Budget{}, err
	}
	if b := st.Budget; b != nil && (len(b.Identities) > 0 || b.GraphQL == nil) && g.Host.Now().Sub(b.At) <= g.Host.Cfg().Merge.BudgetFresh.Duration && g.Host.Now().Before(b.Reset) {
		return minimumGateBudget(*b, g.Host.Now())
	}
	var b github.Budget
	err = g.ReadRetrying("the GitHub budget", func() (err error) {
		ctx, cancel := context.WithTimeout(g.Ctx, 30*time.Second)
		defer cancel()
		b, err = Cmd.ReadBudget(g.Host, ctx)
		return UnsavedBudget(err)
	})
	if err != nil {
		return state.Budget{}, err
	}
	reading := state.Budget{Identity: b.Identity, Remaining: b.Remaining, Limit: b.Limit, Reset: b.Reset}
	if b.GraphQL != nil {
		reading.GraphQL = Cmd.StoredGraphQL(b.GraphQL, g.Host.Now())
	}
	return minimumGateBudget(reading, g.Host.Now())
}

func minimumGateBudget(b state.Budget, now time.Time) (state.Budget, error) {
	if g := b.GraphQL; g != nil {
		if g.Err != "" || g.Blocks(now) {
			return b, fmt.Errorf("GraphQL identity %s is unavailable (blocked by %s): %s %s", g.Identity, strings.Join(g.BlockedBy, ", "), g.Err, g.Refused)
		}
		if g.Remaining < b.Remaining {
			b.Identity, b.Login, b.Remaining, b.Limit, b.Reset = g.Identity, g.Login, g.Remaining, g.Limit, g.Reset
		}
	}
	return b, nil
}

// graphQLSpentUntil says b's GraphQL identity is refused only under a
// spent limit whose reset GitHub named and is still ahead at now: the merge
// waits for that reset in its place (spentWait) instead of being refused. An
// identity blocked otherwise, or unread, stays unknown.
func graphQLSpentUntil(b state.Budget, now time.Time) bool {
	g := b.GraphQL
	return g != nil && g.Err == "" && len(g.BlockedBy) == 0 && g.Refused != "" && now.Before(g.Reset)
}

// UnsavedBudget is a budget read's error without the failures to save the
// reading: when every failure in err is the state lock being busy, GitHub
// answered and only keeping the reading lost to another beekeeper process
// holding the lock for a moment. The reading is known and stands, and the
// next read saves it; any other failure keeps the budget unknown (#3411).
func UnsavedBudget(err error) error {
	if err != nil && OnlyLockBusy(err) {
		return nil
	}
	return err
}

// OnlyLockBusy says every leaf of err, through wrapping and joining, is a
// LockBusyError or coordinator lease commit lock contention.
func OnlyLockBusy(err error) bool {
	switch e := err.(type) {
	case *state.LockBusyError:
		return true
	case interface{ Unwrap() []error }:
		errs := e.Unwrap()
		if len(errs) == 2 && errors.Is(errs[0], execution.ErrCoordinatorBusy) && errors.Is(errs[1], context.DeadlineExceeded) {
			return true
		}
		for _, inner := range errs {
			if !OnlyLockBusy(inner) {
				return false
			}
		}
		return len(errs) > 0
	case interface{ Unwrap() error }:
		return OnlyLockBusy(e.Unwrap())
	}
	return errors.Is(err, execution.ErrCoordinatorBusy)
}

// capRuns counts the machine's devctl merges and promotes, a running
// merge's own once even before its devctl started; the waits only poll and
// do not count.
func capRuns(st *state.State) int {
	running, children := runningMerges(st), map[int]bool{}
	for _, m := range st.Merges {
		if m.Phase == state.Running {
			children[m.Child] = true
		}
	}
	n := len(running)
	t, err := Cmd.Plat().Machine.Processes()
	if err != nil {
		return n
	}
	for _, p := range t.ByPID {
		if p.Comm == merge.Tool && merge.ParseCapped(p.Args) && !running[p.PPID] && !children[p.PPID] {
			n++
		}
	}
	return n
}

// runningMerges are the gates of the running merges, by pid.
func runningMerges(st *state.State) map[int]bool {
	running := map[int]bool{}
	for _, m := range st.Merges {
		if m.Phase == state.Running {
			running[m.PID] = true
		}
	}
	return running
}

// toolVersion is what `devctl version` reports, "" when it does not run.
func toolVersion(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, merge.Tool, "version") //nolint:gosec // the merge tool, a constant
	c.Env = Cmd.DevctlCommandEnv("")
	out, err := c.Output()
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

// toolAcceptsRed says whether the installed merge tool's pr merge has
// --accept-red: its help names the flag.
func toolAcceptsRed(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, merge.Tool, "pr", "merge", "--help") //nolint:gosec // the merge tool, a constant
	c.Env = Cmd.DevctlCommandEnv("")
	out, _ := c.Output()
	return strings.Contains(string(out), merge.AcceptRedFlag)
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

// ReadHelmReleases reads the lane installation's HelmReleases with kubectl,
// each with the range its OCIRepository follows.
var ReadHelmReleases = func(ctx context.Context, lane config.Lane) ([]merge.HelmRelease, error) {
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

func ExitCode(rc int) error {
	if rc == 0 {
		return nil
	}
	return Cmd.NewExitError(rc, "", nil)
}
