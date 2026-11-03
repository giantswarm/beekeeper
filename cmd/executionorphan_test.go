package cmd

import (
	"context"
	"fmt"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/google/uuid"
	"strings"
	"testing"
)

func TestHostsDrainNamesRecordedExecutions(t *testing.T) {
	a, r := executionTestApp(t)
	withCoordinatorVersion(t, hostNewRelease)
	_, err := a.updateExecutionHost(context.Background(), sshTestSelector)
	if err == nil || !strings.Contains(err.Error(), string(r.Ref.Execution)) || !strings.Contains(err.Error(), r.Name) {
		t.Fatalf("drain must name its execution and worker: %v", err)
	}
}

type orphanTransport struct {
	*testExecutionTransport
	refs           []execution.Ref
	absent         bool
	inventoryCalls int
}

func (f *orphanTransport) Inventory(context.Context, string) ([]execution.Ref, error) {
	f.inventoryCalls++
	return f.refs, nil
}
func (f *orphanTransport) Missing(context.Context, execution.Ref) (bool, error) { return f.absent, nil }

func TestExecutionOrphanInventoryStopsOnlyUnknownJournal(t *testing.T) {
	a, r := executionTestApp(t)
	a.cfg.Identity.Host = r.Coordinator
	orphan := r.Ref
	orphan.Execution = execution.ExecutionID(uuid.NewString())
	calls := 0
	fake := &orphanTransport{testExecutionTransport: &testExecutionTransport{stop: func(_ context.Context, ref execution.Ref, q execution.StopRequest) (execution.Ack, error) {
		if ref != orphan {
			t.Fatal("stopped a known execution")
		}
		calls++
		return execution.Ack{ID: q.ID, Stage: execution.Applied}, nil
	}}, refs: []execution.Ref{r.Ref, orphan}}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	h := a.cfg.Agents.Hosts[sshTestSelector]
	if err := a.reconcileHostInventory(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("stops=%d", calls)
	}
	// With no coordinator records at all, the watch must still query the host.
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { st.Starts = nil; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	fake.refs = nil
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	w := &watcher{app: a}
	w.syncExecutions(context.Background(), st)
	if fake.inventoryCalls != 2 {
		t.Fatalf("inventory calls=%d", fake.inventoryCalls)
	}
}

func TestExecutionMissingJournalSettlesOnlyWithInactiveUnit(t *testing.T) {
	for _, absent := range []bool{false, true} {
		t.Run(fmt.Sprint(absent), func(t *testing.T) {
			a, _ := executionTestApp(t)
			syncs := 0
			fake := &orphanTransport{testExecutionTransport: &testExecutionTransport{
				sync: func(context.Context, execution.Ref) (execution.ControlResult, error) {
					syncs++
					return execution.ControlResult{}, execution.ErrNotFound
				},
				inspect: func(context.Context, execution.Ref) (execution.Observation, error) {
					return execution.Observation{}, execution.ErrNotFound
				},
			}, absent: absent}
			a.executionTransport = func(execution.Host) execution.Transport { return fake }
			err := a.syncExecutionHost(context.Background(), sshTestSelector)
			if (err == nil) != absent {
				t.Fatalf("sync=%v, absent=%v", err, absent)
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			rec := st.Starts[0].Execution
			if rec.Terminal() != absent || rec.Settled != absent {
				t.Fatal("missing journal was not settled as expected")
			}
			if rec.Observation.StopAcknowledged {
				t.Fatal("invented stop acknowledgement")
			}
			if absent {
				if err := a.syncExecutionHost(context.Background(), sshTestSelector); err != nil {
					t.Fatal(err)
				}
				if syncs != 1 {
					t.Fatalf("repeated missing journal sync: %d", syncs)
				}
				if hostExecutions(st, sshTestSelector) != 0 {
					t.Fatal("missing inactive execution blocks drain")
				}
			}
		})
	}
}

func TestMissingJournalSettlesPendingStopWithoutAcknowledgement(t *testing.T) {
	a, r := executionTestApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.StopRequest = &execution.StopRequest{ID: "stop-missing", Reason: "stop"}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	fake := &orphanTransport{testExecutionTransport: &testExecutionTransport{}, absent: true}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	h := a.cfg.Agents.Hosts[sshTestSelector]
	if settled, err := a.reconcileMissingExecution(context.Background(), h, r.Ref); err != nil || !settled {
		t.Fatalf("settled=%v %v", settled, err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	rec := st.Starts[0].Execution
	if !rec.Terminal() || rec.Observation.StopAcknowledged || executionNeedsSync(st, rec) {
		t.Fatal("lost execution with pending stop was not settled honestly")
	}
}

func TestExecutionWatchFindsHostOnlyJournal(t *testing.T) {
	a, r := executionTestApp(t)
	a.cfg.Identity.Host = r.Coordinator
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { st.Starts = nil; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	stops := 0
	fake := &orphanTransport{testExecutionTransport: &testExecutionTransport{stop: func(_ context.Context, ref execution.Ref, q execution.StopRequest) (execution.Ack, error) {
		if ref != r.Ref {
			t.Fatal("unexpected orphan identity")
		}
		stops++
		return execution.Ack{ID: q.ID, Stage: execution.Applied}, nil
	}}, refs: []execution.Ref{r.Ref}}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	w := &watcher{app: a}
	w.syncExecutions(context.Background(), st)
	if stops != 1 {
		t.Fatalf("host-only journal stops=%d; expected one", stops)
	}
}

func TestMissingJournalDoesNotReleaseAnInflightStart(t *testing.T) {
	a, r := executionTestApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Phase = executionPending
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	fake := &orphanTransport{testExecutionTransport: &testExecutionTransport{}, absent: true}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	if settled, err := a.reconcileMissingExecution(context.Background(), a.cfg.Agents.Hosts[sshTestSelector], r.Ref); err != nil || settled {
		t.Fatalf("inflight start settled=%v %v", settled, err)
	}
}
