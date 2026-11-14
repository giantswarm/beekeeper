package cmd

import (
	"context"
	"errors"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

const executionRetireEmitKey = "execution-retire"

// executionRetireBatch bounds one tick's retirements: the backlog of a
// coordinator's first tick on this release must not hold up its watch.
const executionRetireBatch = 8

// executionRetirementTrigger is why r retires now, "" while it does not. Every
// terminal path enters the same retirement. A stop counts from its request, so
// that revocation does not wait for a host that cannot be reached; the host's
// part waits for the terminal state (executionHostRetirable).
func executionRetirementTrigger(st *state.State, r *state.ExecutionRecord) string {
	switch {
	case r.Retirement != nil:
		return r.Retirement.Trigger
	case r.StopRequest != nil:
		return execution.RetireTriggerStop
	case executionUnfinished(st, r):
		return ""
	case r.Released():
		return execution.RetireTriggerFailedStart
	case r.Completed:
		return execution.RetireTriggerReport
	case !r.Terminal():
		return ""
	case !r.Observation.Ready:
		return execution.RetireTriggerFailedStart
	}
	return execution.RetireTriggerCrash
}

// retireEndedExecutions advances every retirement that is due, each on its own
// backoff and independently of the host's reconciliation schedule: a host
// that stays down never holds back another host's retirement or any revocation.
func (w *watcher) retireEndedExecutions(ctx context.Context, st *state.State) *state.State {
	now := w.a.now
	if now.IsZero() {
		now = time.Now()
	}
	var failures []error
	down := map[string]bool{}
	ran := 0
	for _, s := range st.Starts {
		r := s.Execution
		if ran == executionRetireBatch || ctx.Err() != nil {
			break
		}
		if r == nil || r.Retirement.Finished() {
			continue
		}
		trigger := executionRetirementTrigger(st, r)
		if trigger == "" || r.Retirement != nil && now.Before(r.Retirement.NextAttempt) {
			continue
		}
		// Revoked and waiting for the host's part: nothing to do this tick.
		if r.Retirement != nil && !r.Retirement.RevokedAt.IsZero() && !w.a.executionHostRetirable(st, r) {
			continue
		}
		ran++
		// A host on an older release keeps its retirement open on the backoff;
		// it is not a failure to raise until the host is updated.
		if err := w.a.retireExecution(ctx, r.Handle.Ref, trigger, now, down[r.Selector]); err != nil && !errors.Is(err, execution.ErrUnsupported) {
			failures = append(failures, err)
			if isUnreachable(err) {
				down[r.Selector] = true
			}
		}
	}
	if len(failures) > 0 {
		w.mu.Lock()
		if w.last == nil {
			w.last = map[string]time.Time{}
		}
		w.mu.Unlock()
		w.emit(executionRetireEmitKey, "EXECUTION RETIREMENT UNCONFIRMED: %d of them: %v", len(failures), failures[0])
	} else if ran > 0 {
		w.clear(executionRetireEmitKey)
	}
	if ran == 0 {
		return st
	}
	if updated, err := w.a.store.Read(); err == nil {
		return updated
	}
	return st
}
