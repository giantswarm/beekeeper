package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	// executionNothingOnHost is the stage of a reservation no start ever left
	// the coordinator for: there is nothing on its host to retire.
	executionNothingOnHost = "nothing-on-host"
	executionRevokeTimeout = 20 * time.Second
)

// executionUnfinished says the worker of r may still go on: it is parked, its
// desk review is open, or it finished a round and not the task. Its worktree
// and thread stay, whatever its unit does between turns.
func executionUnfinished(st *state.State, r *state.ExecutionRecord) bool {
	ref := r.Handle.Ref
	// A fixer inherits the review record, but has not parked its own round.
	// Only a completed round is kept by that record; an unreported crash
	// must retire so the next repair can replace its ended execution.
	if r.Completed && slices.ContainsFunc(st.ReviewParks, func(p state.ReviewPark) bool { return p.Execution != nil && *p.Execution == ref }) {
		return true
	}
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Execution != nil && *ag.Execution == ref })
	if i < 0 {
		return false
	}
	return st.Agents[i].Park != nil || r.Completed && !st.Agents[i].Done
}

// executionHostRetirable says the host's part may go ahead: the execution
// ended, its log reached the transcript store and no publication still needs
// it. A desk stop holds this part and never revocation.
func (a *app) executionHostRetirable(st *state.State, r *state.ExecutionRecord) bool {
	if st.Stop != nil || !r.Terminal() || executionUnfinished(st, r) && r.StopRequest == nil {
		return false
	}
	return r.HostJournalMissing || !executionTranscriptPending(r) && !a.hasUnrecoveredPublication(r)
}

// retireExecution takes one step of ref's retirement: intent is durable first,
// then revocation and the host's part each run on their own, and only what
// was confirmed is recorded as done. skipHost leaves the host's part to a later
// attempt, for a host another execution just found unreachable.
func (a *app) retireExecution(ctx context.Context, ref execution.Ref, trigger string, now time.Time, skipHost bool) error {
	var rec *state.ExecutionRecord
	hostReady := false
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		r, i := executionRecord(st, ref)
		if r == nil {
			return nil, refused("execution is not owned")
		}
		var events []state.Event
		if r.Retirement == nil {
			r.Retirement = &state.ExecutionRetirement{ID: uuid.NewString(), Trigger: trigger, RequestedAt: now.UTC()}
			events = append(events, event(st.Starts[i].By, "execution.retirement-requested", "%s: %s on %s", st.Starts[i].Name, trigger, ref.Host))
		}
		copied := *r
		rec, hostReady = &copied, a.executionHostRetirable(st, r)
		return events, nil
	}); err != nil {
		return err
	}
	retirement := *rec.Retirement
	var revokeErr, hostErr error
	if retirement.RevokedAt.IsZero() {
		revokeErr = a.confirmExecutionRevoked(ctx, rec)
	}
	var result *execution.Retirement
	if retirement.RetiredAt.IsZero() && !skipHost && hostReady {
		result, hostErr = a.retireOnHost(ctx, rec, retirement)
	}
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		r, i := executionRecord(st, ref)
		if r == nil || r.Retirement == nil {
			return nil, refused("execution retirement disappeared")
		}
		x := r.Retirement
		var events []state.Event
		if retirement.RevokedAt.IsZero() {
			if revokeErr == nil {
				x.RevokedAt, x.RevokeError = now.UTC(), ""
				if r.Request.GitHub {
					events = append(events, event(st.Starts[i].By, "execution.authorization-revoked", "%s: worker GitHub authorization revoked", st.Starts[i].Name))
				}
			} else {
				x.RevokeError = oneLine(revokeErr.Error())
			}
		}
		switch {
		case hostErr != nil:
			x.HostError = oneLine(hostErr.Error())
		case result != nil:
			x.RetiredAt, x.Stage, x.Recovery, x.RetainUntil, x.HostError = now.UTC(), result.Stage, result.Recovery, result.RetainUntil, ""
			events = append(events, event(st.Starts[i].By, "execution.retired", "%s: %s on %s: %s", st.Starts[i].Name, x.Trigger, ref.Host, retirementOutcome(result)))
		}
		if revokeErr != nil || hostErr != nil {
			x.Attempts++
			x.NextAttempt = now.UTC().Add(executionSyncBackoff(x.Attempts))
		} else {
			x.Attempts, x.NextAttempt = 0, time.Time{}
		}
		return events, nil
	})
	return errors.Join(revokeErr, hostErr, err)
}

func retirementOutcome(r *execution.Retirement) string {
	if r.Recovery != nil {
		return fmt.Sprintf("work preserved as %s in %s", r.Recovery.Commit, r.Recovery.Bundle)
	}
	return "no unpublished commits or edits, stage " + r.Stage
}

// confirmExecutionRevoked revokes on a fresh deadline, so a caller out of time
// for its host still ends the worker's authorization. Only success confirms it.
func (a *app) confirmExecutionRevoked(ctx context.Context, r *state.ExecutionRecord) error {
	if !r.Request.GitHub {
		return nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), executionRevokeTimeout)
	defer cancel()
	return a.revokeWorkerGitHub(cleanup, r.Handle.Ref)
}

// retireOnHost asks the host for its part, nil without an answer: a transport
// predating the operation holds the retirement open rather than failing it.
func (a *app) retireOnHost(ctx context.Context, r *state.ExecutionRecord, x state.ExecutionRetirement) (*execution.Retirement, error) {
	ref := r.Handle.Ref
	if r.Released() && r.StartUnsent {
		return &execution.Retirement{Ref: ref, Stage: executionNothingOnHost}, nil
	}
	h, err := a.executionHost(r.Selector)
	if err != nil {
		return nil, err
	}
	if h.Identity != ref.Host {
		return nil, refused("recorded host identity changed")
	}
	retirer, ok := a.executionClient(h).(execution.Retirer)
	if !ok {
		return nil, nil
	}
	opCtx, cancel := context.WithTimeout(ctx, executionOperationTimeout(h))
	defer cancel()
	result, err := retirer.Retire(opCtx, ref, execution.RetireRequest{ID: x.ID, Trigger: x.Trigger, Reason: "execution ended: " + x.Trigger})
	if err != nil {
		if isUnreachable(err) {
			a.executionFailure(ref, err)
		}
		return nil, fmt.Errorf("retire %s: %w", ref.Execution, err)
	}
	if result.Ref != ref || !result.Finished() {
		return nil, fmt.Errorf("retire %s: host did not confirm retirement", ref.Execution)
	}
	return &result, nil
}
