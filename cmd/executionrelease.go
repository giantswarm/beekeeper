package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

// unstartedReleaseAfter is how long a reservation its host never
// acknowledged stays the starting process's own: past it, the loop's host
// sync releases one that no host journal backs.
const unstartedReleaseAfter = 15 * time.Minute

// executionReleasedEvent logs the release of a reservation its host never
// acknowledged: the start it was made for ran nothing.
const executionReleasedEvent = "execution.released"

// preparePendingExecution takes a reservation its host has not yet
// acknowledged to a registered handle: it rebinds it to the host's current
// policy, pins its source and sends the start. sentAt is the start send the
// reservation records when it returns, zero while none left the
// coordinator.
func (a *app) preparePendingExecution(ctx context.Context, client execution.Transport, host execution.Host, rec *state.ExecutionRecord) (execution.StartRequest, time.Time, error) {
	ref, r, sentAt := rec.Handle.Ref, rec.Request, rec.StartSentAt
	// Absence is known when no start left the coordinator, or confirmed
	// by the host; an ambiguous inspection never counts as absence.
	absent := rec.StartUnsent
	if r.PolicyDigest != host.PolicyDigest {
		if !absent {
			_, inspectErr := client.Inspect(ctx, ref)
			switch {
			case inspectErr == nil:
				return r, sentAt, refused("execution host holds this reservation under its earlier policy; stop it and start a new one")
			case !errors.Is(inspectErr, execution.ErrNotFound):
				a.executionFailure(ref, inspectErr)
				return r, sentAt, fmt.Errorf("cannot confirm the reservation never reached its host: %w", inspectErr)
			}
			absent = true
		}
		var err error
		if r, err = a.rebindExecution(ctx, host, ref); err != nil {
			return r, sentAt, err
		}
	}
	if uploader, ok := client.(execution.SourceUploader); ok {
		// A pending coordinator may have lost a legacy preparation reply.
		// Preserve its original request/digest whenever the host journal
		// already exists. Only confirmed absence permits source pinning.
		existing := false
		if r.SourceDigest == "" && !absent {
			_, inspectErr := client.Inspect(ctx, ref)
			switch {
			case inspectErr == nil:
				existing = true
			case errors.Is(inspectErr, execution.ErrNotFound):
			default:
				a.executionFailure(ref, inspectErr)
				return r, sentAt, fmt.Errorf("cannot reconcile pending preparation: %w", inspectErr)
			}
		}
		if !existing {
			var data []byte
			var err error
			var basis execution.SourceBasis
			if reader, ok := client.(execution.SourceBasisReader); ok && r.SourceDigest == "" {
				// An older host answers no basis: it receives the whole history.
				basis, _ = reader.SourceBasis(ctx, ref, r.Repository)
			}
			r, data, err = a.prepareExecutionSource(ctx, r, basis.Commits)
			if err != nil {
				return r, sentAt, err
			}
			if err := uploader.UploadSource(ctx, ref, data); err != nil {
				return r, sentAt, err
			}
		}
	}
	// The send is durable before it leaves: from here the host may hold a
	// journal, and only its confirmed absence releases the reservation.
	sent := time.Now().UTC()
	if err := a.store.Update(func(current *state.State) ([]state.Event, error) {
		x, _ := executionRecord(current, ref)
		if x == nil || x.Released() || x.Phase != executionPending || !x.StartSentAt.Equal(sentAt) {
			return nil, refused("execution reservation changed before its start was sent")
		}
		if err := a.refuseStopped(current, "starting an SSH execution"); err != nil {
			return nil, err
		}
		if x.StopRequest != nil {
			return nil, refused("execution has a stop request")
		}
		x.StartUnsent, x.StartSentAt = false, sent
		return nil, nil
	}); err != nil {
		return r, sentAt, err
	}
	sentAt = sent
	handle, err := client.Start(ctx, r)
	if err != nil {
		a.executionFailure(ref, err)
		return r, sentAt, fmt.Errorf("remote preparation failed or was interrupted: %w; recover the recorded execution", err)
	}
	if err := execution.ValidateHandle(host, r, handle); err != nil {
		return r, sentAt, err
	}
	err = a.store.Update(func(current *state.State) ([]state.Event, error) {
		x, i := executionRecord(current, ref)
		if x == nil {
			return nil, refused("registration disappeared")
		}
		if err := a.refuseStopped(current, "activating an SSH execution"); err != nil {
			return nil, err
		}
		if x.StopRequest != nil {
			return nil, refused("execution has a stop request")
		}
		x.Handle, x.Phase, current.Starts[i].Dir = handle, execution.Prepared, handle.Workspace
		return []state.Event{event(current.Starts[i].By, "execution.prepared", "%s: registered handle on %s", current.Starts[i].Name, ref.Host)}, nil
	})
	return r, sentAt, err
}

// rebindExecution binds a reservation its host never acknowledged to the
// host's current policy, checked against the host before it is recorded.
func (a *app) rebindExecution(ctx context.Context, host execution.Host, ref execution.Ref) (execution.StartRequest, error) {
	if _, err := a.probeExecution(ctx, host); err != nil {
		return execution.StartRequest{}, err
	}
	var r execution.StartRequest
	err := a.store.Update(func(current *state.State) ([]state.Event, error) {
		x, i := executionRecord(current, ref)
		if x == nil || x.Released() || x.Phase != executionPending || x.StopRequest != nil {
			return nil, refused("execution reservation changed before its policy was rebound")
		}
		if err := a.refuseStopped(current, "rebinding an SSH execution"); err != nil {
			return nil, err
		}
		was := x.Request.PolicyDigest
		x.Request.PolicyDigest = host.PolicyDigest
		x.RequestDigest = x.Request.Digest()
		r = x.Request
		return []state.Event{event(current.Starts[i].By, "execution.rebound", "%s: host %s policy %s, was %s", current.Starts[i].Name, ref.Host, shortDigest(host.PolicyDigest), shortDigest(was))}, nil
	})
	return r, err
}

func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19]
	}
	return d
}

// releaseFailedStart releases the reservation of a start that failed
// before its host acknowledged it: one whose start never left the
// coordinator, or one the host answered with an error and holds no journal
// of. A start whose reply was lost may still run on the host and stays
// reserved for recovery.
func (a *app) releaseFailedStart(ctx context.Context, client execution.Transport, ref execution.Ref, sentAt time.Time, failure error) (bool, error) {
	st, err := a.store.Read()
	if err != nil {
		return false, err
	}
	rec, _ := executionRecord(st, ref)
	if rec == nil || rec.Released() || rec.Phase != executionPending || !rec.StartSentAt.Equal(sentAt) {
		return false, nil
	}
	if !rec.StartUnsent {
		if isUnreachable(failure) {
			return false, nil
		}
		if _, err := client.Inspect(ctx, ref); !errors.Is(err, execution.ErrNotFound) {
			return false, nil
		}
	}
	return a.releaseUnstarted(ref, sentAt, oneLine(failure.Error()))
}

// releaseStaleReservation releases a reservation its host never
// acknowledged once unstartedReleaseAfter passed since its reservation or
// last start send, when no host journal backs it. Before that, or while the
// host may hold its journal, it stays.
func (a *app) releaseStaleReservation(ctx context.Context, client execution.Transport, rec *state.ExecutionRecord, now time.Time) (bool, error) {
	if rec.Released() || rec.Phase != executionPending {
		return false, nil
	}
	if now.Sub(rec.ReservedAt) < unstartedReleaseAfter || now.Sub(rec.StartSentAt) < unstartedReleaseAfter {
		return false, nil
	}
	ref := rec.Handle.Ref
	why := "its start was never sent"
	if !rec.StartUnsent {
		_, err := client.Inspect(ctx, ref)
		switch {
		case errors.Is(err, execution.ErrNotFound):
			why = "its host holds no journal of it"
		case err != nil:
			return false, err
		default:
			return false, nil
		}
	}
	return a.releaseUnstarted(ref, rec.StartSentAt, fmt.Sprintf("unacknowledged for %s: %s", unstartedReleaseAfter, why))
}

// stopUnactivated stops an execution whose host holds it prepared past
// unstartedReleaseAfter since its start was sent: a start that timed out and
// that nobody retried. No process runs, so nothing is lost, its workspace
// stays, and it holds no host slot or update from then on. Only the host's own
// answer counts, and a start still inside its window stays the starter's.
func (a *app) stopUnactivated(ctx context.Context, client execution.Transport, rec *state.ExecutionRecord, now time.Time) (bool, error) {
	if !unactivated(rec) || rec.StartUnsent || now.Sub(rec.StartSentAt) < unstartedReleaseAfter {
		return false, nil
	}
	ref := rec.Handle.Ref
	observation, err := client.Inspect(ctx, ref)
	if err != nil {
		if errors.Is(err, execution.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if observation.ProcessState != execution.Prepared {
		return false, nil
	}
	return true, a.stopExecution(ctx, ref, fmt.Sprintf("start never activated within %s", unstartedReleaseAfter))
}

// releaseStoppedReservation settles the stop of a reservation its host
// never acknowledged, done once it is decided: released when no start left
// the coordinator, or when the host holds no journal of a start sent past
// unstartedReleaseAfter; refused while a start sent inside that window may
// still land. A host journal, or an inspection that cannot tell, leaves the
// stop to the host (done false).
func (a *app) releaseStoppedReservation(ctx context.Context, client execution.Transport, rec *state.ExecutionRecord, now time.Time) (bool, error) {
	ref := rec.Handle.Ref
	if rec.StartUnsent {
		return a.releaseUnstarted(ref, rec.StartSentAt, "stopped before its start was sent")
	}
	if _, err := client.Inspect(ctx, ref); !errors.Is(err, execution.ErrNotFound) {
		return false, nil
	}
	if now.Sub(rec.StartSentAt) < unstartedReleaseAfter {
		return true, refused("its start sent at %s has no host journal yet and may still land; stop it again after %s", rec.StartSentAt.Format(time.RFC3339), rec.StartSentAt.Add(unstartedReleaseAfter).Format(time.RFC3339))
	}
	return a.releaseUnstarted(ref, rec.StartSentAt, "stopped; its host holds no journal of it")
}

// releaseUnstarted gives up a reservation its host never acknowledged: it
// holds no host slot (Terminal) and its agent leaves the roster, so it
// holds no desk slot and its name and board item are free for a new start.
// sentAt guards against a start sent since the caller's reading.
func (a *app) releaseUnstarted(ref execution.Ref, sentAt time.Time, reason string) (bool, error) {
	released := false
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		x, i := executionRecord(st, ref)
		if x == nil || x.Released() || x.Phase != executionPending || !x.StartSentAt.Equal(sentAt) {
			return nil, nil
		}
		x.ReleasedAt, x.ReleaseReason = time.Now().UTC(), reason
		released = true
		if j := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Execution != nil && *ag.Execution == ref }); j >= 0 {
			party := st.Agents[j].Party
			removeAgent(st, j)
			st.Records = slices.DeleteFunc(st.Records, func(r state.Record) bool { return r.Session.Is(party) })
		}
		return []state.Event{event(st.Starts[i].By, executionReleasedEvent, "%s: host %s never acknowledged its start: %s", st.Starts[i].Name, ref.Host, reason)}, nil
	})
	return released, err
}
