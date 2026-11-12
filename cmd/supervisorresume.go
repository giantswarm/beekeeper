package cmd

import (
	"context"
	"errors"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
)

var loopResumePark = (*app).resumeParked

func endedParkCandidate(ag state.Agent) bool {
	_, _, nonClaude := harnessForHost(ag.HostSession)
	return nonClaude && !ag.Done && ag.Task != "" && ag.Execution == nil && ag.Park != nil && !ag.Park.Resumable.IsZero() && len(ag.Park.Continuation) == 0 && !ag.Park.Continuing
}

// Persistent inbox harnesses do not run the Claude stop-post hook. Resume
// only a positively observed ended turn, once per observed turn timestamp.
func loopRecoverUnfinishedTurns(l *Loop, ctx context.Context, st *state.State) error {
	if l.DryRun || st.Stop != nil || l.HoldingStarts() {
		return nil
	}
	sessions, _, err := freshSessions(asApp(l.Host))
	if err != nil {
		return err
	}
	for _, ag := range st.Agents {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		h, _, ok := harnessForHost(ag.HostSession)
		if !ok && ag.Execution == nil || ag.Done || ag.Task == "" || ag.Park != nil || roleRun(st, ag.Party) || !ag.ContinuationExhausted.IsZero() {
			continue
		}
		if keptBy(st, ag, asApp(l.Host).now) != "" || agyQuotaError(asApp(l.Host).cfg, ag) != nil {
			continue
		}
		if ag.Execution != nil {
			r, _ := executionRecord(st, *ag.Execution)
			if r == nil || !r.Request.Messaging || r.Completed || r.StopRequest != nil || r.Observation.StopAcknowledged || !r.Observation.Ready || r.Observation.Reachability != execution.Reachable || executionInTurn(st, ag.Party, asApp(l.Host).now) {
				continue
			}
			if h, ok := asApp(l.Host).cfg.Agents.Hosts[r.Selector]; !ok || h.Identity != r.Handle.Ref.Host {
				continue
			}
			o := r.Observation
			if !o.BetweenTurns() || o.LastActive.IsZero() || !ag.ResumedWait.IsZero() && !o.LastActive.After(ag.ResumedWait) {
				continue
			}
			if _, err := asApp(l.Host).resumeOnWait(ctx, ag.Name, ""); err != nil && !isUnreachable(err) && !errors.Is(err, execution.ErrUnsupported) {
				return err
			}
			continue
		}
		endedAt := ag.AssignedAt
		ended := false
		for _, s := range sessions {
			if s.Party().Is(ag.Party) && s.State == omp.StateIdle && len(s.Commands) == 0 {
				ended, endedAt = true, s.LastActive
			}
		}
		if !ended {
			if _, live := liveSession(sessions, ag.Party); live {
				continue
			}
			for _, s := range h.Ended(asApp(l.Host).cfg, sessions, ag.AssignedAt) {
				if s.Party().Is(ag.Party) {
					ended, endedAt = true, s.LastActive
				}
			}
		}
		if !ended || endedAt.IsZero() || !ag.ResumedWait.IsZero() && !endedAt.After(ag.ResumedWait) {
			continue
		}
		if _, err := asApp(l.Host).resumeOnWait(ctx, ag.HostSession, ""); err != nil {
			return err
		}
	}
	return nil
}
