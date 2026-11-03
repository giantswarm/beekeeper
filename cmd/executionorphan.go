package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

// reconcileHostInventory stops only host-journal-backed executions of this
// coordinator whose full identity is absent from its current durable records.
// It never adopts a process or re-creates a removed worker registration.
func (a *app) reconcileHostInventory(ctx context.Context, h execution.Host) error {
	client := a.executionClient(h)
	inventory, ok := client.(execution.InventoryReader)
	if !ok || !execution.ValidIdentity(a.cfg.Identity.Host) {
		return nil
	}
	refs, err := inventory.Inventory(ctx, a.cfg.Identity.Host)
	if errors.Is(err, execution.ErrUnsupported) {
		return nil
	} // pre-inventory host
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		return nil
	}
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if ref.Validate() != nil || ref.Host != h.Identity {
			return errors.New("host inventory identity mismatch")
		}
		if r, _ := executionRecord(st, ref); r != nil {
			continue
		}
		request := execution.StopRequest{ID: fmt.Sprintf("orphan-%s-%d", ref.Execution, ref.Generation), Reason: "coordinator no longer records this execution"}
		ack, err := client.Stop(ctx, ref, request)
		if err != nil {
			return fmt.Errorf("orphan %s: %w", ref.Execution, err)
		}
		if ack.Stage != execution.Applied || ack.ID != request.ID {
			return fmt.Errorf("orphan %s termination unconfirmed", ref.Execution)
		}
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			return []state.Event{event(state.Party{}, "execution.orphan-reconciled", "stopped journal-backed execution %s on %s absent from coordinator records", ref.Execution, ref.Host)}, nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) reconcileMissingExecution(ctx context.Context, h execution.Host, ref execution.Ref) (bool, error) {
	st, err := a.store.Read()
	if err != nil {
		return false, err
	}
	rec, _ := executionRecord(st, ref)
	// A pending start may still land after the absence probe. Its existing
	// bounded stale-reservation protocol, not this reconciliation, releases it.
	if rec == nil || rec.Phase == executionPending {
		return false, nil
	}
	client := a.executionClient(h)
	inventory, ok := client.(execution.InventoryReader)
	if !ok {
		return false, nil
	}
	absent, err := inventory.Missing(ctx, ref)
	if errors.Is(err, execution.ErrUnsupported) {
		if _, inspectErr := client.Inspect(ctx, ref); errors.Is(inspectErr, execution.ErrNotFound) {
			_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
				if r, _ := executionRecord(st, ref); r != nil && r.Phase != executionPending && !r.HostJournalMissing {
					r.HostJournalUnconfirmed = true
				}
				return nil, nil
			})
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !absent {
		_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
			if r, _ := executionRecord(st, ref); r != nil {
				r.HostJournalUnconfirmed = false
			}
			return nil, nil
		})
		return false, nil
	}
	settled := false
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		r, i := executionRecord(st, ref)
		if r == nil || r.Phase == executionPending {
			return nil, nil
		}
		settled = true
		if r.HostJournalMissing {
			return nil, nil
		}
		now := time.Now().UTC()
		r.Phase, r.Settled, r.HostJournalMissing, r.HostJournalUnconfirmed = execution.Failed, true, true, false
		r.Observation = execution.Observation{Handle: r.Handle, ProcessState: execution.Failed, Reachability: execution.Reachable, ObservedAt: now, ReachabilityAt: now, LastError: "host journal missing; owned unit confirmed inactive"}
		return []state.Event{event(st.Starts[i].By, "execution.missing-reconciled", "%s: execution %s has no host journal and its unit is inactive", st.Starts[i].Name, ref.Execution)}, nil
	})
	return settled && err == nil, err
}
