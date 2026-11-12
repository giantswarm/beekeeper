package cmd

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// loopBroker puts the sandbox's broker onto the binary installed at the
// loop's path and says so: errLoopLater while it serves calls a restart
// would end (as self-update leaves it, its restart due).
var loopBroker = func(ctx context.Context) (string, error) {
	pid, bin, err := brokerProcess()
	if err != nil {
		return "no broker runs", nil //nolint:nilerr // no broker unit: nothing to put on the new binary
	}
	exe, err := ExecutablePath()
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return "", err
	}
	t, err := proc.Read()
	if err != nil {
		return "", err
	}
	switch step, calls := brokerDue(pid, bin, exe, fi.ModTime(), t); step {
	case brokerBusy:
		return fmt.Sprintf("the broker serves %d call(s)", calls), ErrLoopLater
	case brokerRestart:
		if err := restartBroker(ctx); err != nil {
			return "", fmt.Errorf("restarting the broker: %w", err)
		}
		return "the broker restarted on " + exe, nil
	}
	return "the broker runs the installed binary", nil
}

// loopInTurn reports whether p's session runs a turn.
var loopInTurn = func(a *app, p state.Party) bool {
	a = a.snapshotAtNow()
	if a.executionInTurn(p) {
		return true
	}
	_, ok := a.runningTurn(p)
	return ok
}

func (a *app) executionInTurn(p state.Party) bool {
	st, err := a.store.Read()
	if err != nil {
		return false
	}
	return executionInTurn(st, p, a.timeNow())
}

// executionInTurn reports whether p's managed SSH execution is in a turn:
// the host observation reports an active turn, a wake message is in flight,
// or the coordinator recorded turn activity within stewardQuiet while the
// worker is neither parked nor finished.
func executionInTurn(st *state.State, p state.Party, now time.Time) bool {
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Execution != nil && ag.Is(p) })
	if i < 0 {
		return false
	}
	ag := st.Agents[i]
	r, _ := executionRecord(st, *ag.Execution)
	if r == nil || r.Terminal() || r.StopRequest != nil {
		return false
	}
	if r.Observation.InTurn {
		return true
	}
	for _, m := range r.Messages {
		if m.Stage == execution.Uncertain || (!r.Completed && !ag.Done && (m.Stage == executionPending || m.Stage == execution.Stored)) {
			return true
		}
	}
	if ag.Park != nil || r.Completed || ag.Done || now.IsZero() {
		return false
	}
	last := r.LastActive
	if r.Observation.LastActive.After(last) {
		last = r.Observation.LastActive
	}
	if r.Published != nil && r.Published.At.After(last) {
		last = r.Published.At
	}
	return !last.IsZero() && now.Sub(last) < stewardQuiet
}

// loopSessions reads the running Claude sessions and process table for a pass.
var loopSessions = func(a *app) ([]*claude.Session, *proc.Table, error) {
	return a.sessions()
}

// loopRoster runs the doctor's roster and archive step for a pass: it stops left
// sleeping waits, takes off the roster any finished agent (done, relieved or
// stale with no CLI) that superloop.Plan is not still handling in planned, and
// archives or stops owed desktop and background sessions once their turns end.
func loopRoster(l *Loop, ctx context.Context, planned map[string]bool) error {
	if l.PauseForReexec() || ctx.Err() != nil {
		return nil
	}
	a := asApp(l.Host).snapshotAtNow()
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	sessions, t, err := loopSessions(a)
	if err != nil {
		return nil //nolint:nilerr // process or session read failure does not fail the loop pass
	}
	record := func(host string) (*claude.Record, bool) { return claude.ReadRecord(a.cfg, host) }
	inFlight := func(p state.Party) bool { return mergeInFlight(st, p, proc.Alive) }
	waits := sleepingWaits(st, sessions, t, inFlight, a.timeNow())
	for _, w := range waits {
		if l.PauseForReexec() || ctx.Err() != nil {
			return nil
		}
		stopProcesses(w.pids())
		line := "stopped " + w.String()
		_ = a.store.Update(func(*state.State) ([]state.Event, error) {
			return []state.Event{event(LoopParty, "agents.stop", "%s", line)}, nil
		})
	}
	if l.PauseForReexec() || ctx.Err() != nil {
		return nil
	}
	busy := func(p state.Party) bool {
		if slices.ContainsFunc(waits, func(w leftWait) bool { return w.agent.Is(p) }) {
			return false
		}
		_, turn := turnRunning(sessions, t, p, a.timeNow())
		return turn || loopInTurn(a, p) || inFlight(p)
	}
	chores := slices.DeleteFunc(planChores(st, sessions, record, busy, a.cfg.Agents.StaleAfter.Duration, a.timeNow(), a.cfg), func(c chore) bool {
		return c.kind != choreRemove || planned[c.agent.Name]
	})
	_, err = a.finishRoster(ctx, chores, sessions, record, busy, LoopParty, "beekeeper supervisor loop")
	return err
}

// readIntakeAnchor resolves the board item that ref should be ranked right
// after (Client.RankAnchor).
var readIntakeAnchor = func(ctx context.Context, a *app, ref, from string) (string, error) {
	return a.boardClient().RankAnchor(ctx, ref, from)
}

// loopFiledIssues reads the issues created by sessions active since the loop
// started (Work.Created), with the item or pull request each session was
// working on when it filed the issue.
func loopFiledIssues(l *Loop, st *state.State) []superloop.FiledIssue {
	sessions, _, err := loopSessions(asApp(l.Host))
	if err != nil || len(sessions) == 0 {
		return nil
	}
	var out []superloop.FiledIssue
	for _, s := range sessions {
		if !s.LastActive.IsZero() && s.LastActive.Before(l.Since) {
			continue
		}
		w, _ := harnessForSession(s).ReadTranscript(asApp(l.Host), s, asApp(l.Host).timeNow())
		if len(w.Created) == 0 {
			continue
		}
		p := s.Party()
		agent := p.Name
		from := ""
		if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) }); i >= 0 {
			agent = st.Agents[i].Name
			from = AgentRef(st, agent)
		} else if st.Guide != nil && st.Guide.Holder != nil && st.Guide.Holder.Is(p) {
			agent = guideName
		}
		if from == "" {
			for _, list := range [][]string{w.Refs, w.Mentioned} {
				for _, r := range list {
					if !slices.ContainsFunc(w.Created, func(c string) bool { return strings.EqualFold(c, r) }) {
						from = r
						break
					}
				}
				if from != "" {
					break
				}
			}
		}
		var createdRefs []string
		for _, c := range w.Created {
			r, ok := parseRef(c)
			if !ok {
				continue
			}
			ref := refName(r)
			createdRefs = append(createdRefs, ref)
			url := fmt.Sprintf("https://github.com/%s/issues/%d", r.Repo, r.N)
			out = append(out, superloop.FiledIssue{Ref: ref, URL: url, Agent: agent, From: from})
		}
		if !l.DryRun && len(createdRefs) > 0 && from == "" && agent != guideName && !holdsRole(st, p) && !agentIdle(st, p) && !endedOwner(st, p) {
			_ = asApp(l.Host).store.Update(func(current *state.State) ([]state.Event, error) {
				return recordSessionIssues(current, p, p, asApp(l.Host).timeNow(), createdRefs), nil
			})
		}
	}
	return out
}

// loopFireTimers takes only timers with wakes or commands; watch retains line-only
// reminders and their desktop notifications. Checks run outside the state
// lock; settling inside Update lets a concurrent watch take a timer first
// without either firing it twice. Side effects follow the successful save.
func loopFireTimers(l *Loop, ctx context.Context, st *state.State) error {
	a := asApp(l.Host)
	var acting []state.Timer
	for _, t := range st.Timers {
		if timerActs(t) {
			acting = append(acting, t)
		}
	}
	githubHeld := BoardBudgetLow(st.Budget, a.cfg.GitHub.Floor, a.timeNow()) ||
		a.cfg.GitHub.Floor > 0 && LoopBudgetUnknownOrLow(st.Budget, a.cfg.GitHub.Floor, a.timeNow())
	found := a.loopTimerChecks(ctx, acting, a.timeNow(), githubHeld || a.resetFirst(st))
	// Avoid a write on every idle loop pass.
	_, _, _, changed := settleTimersMatching(st, found, a.timeNow(), a.cfg.Supervisor.LoopInterval.Duration, timerActs)
	if !changed {
		return nil
	}
	var lines []string
	var fires []timerFire
	if err := a.store.Update(func(current *state.State) ([]state.Event, error) {
		var evs []state.Event
		lines, evs, fires, _ = settleTimersMatching(current, found, a.timeNow(), a.cfg.Supervisor.LoopInterval.Duration, timerActs)
		for _, line := range lines {
			evs = append(evs, event(watchParty, "watch.timer", "%s", line))
		}
		return evs, nil
	}); err != nil {
		return err
	}
	// Each batch keeps its own time while later passes advance the clock.
	w := &watcher{app: a.snapshotAtNow()}
	for _, line := range lines {
		w.outputEvent(line)
	}
	timerCtx := ctx
	if !l.Once {
		timerCtx = l.BackgroundContext(ctx)
	}
	l.TimerActs.Go(func() {
		w.actTimers(timerCtx, fires)
		w.timerActs.Wait()
	})
	return nil
}
