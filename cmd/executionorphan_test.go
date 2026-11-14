package cmd

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
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
	w := &Watcher{Host: a}
	w.SyncExecutions(context.Background(), st)
	if fake.inventoryCalls != 2 {
		t.Fatalf("inventory calls=%d", fake.inventoryCalls)
	}
	w.Host.now = w.Host.now.Add(time.Minute)
	w.SyncExecutions(context.Background(), st)
	if fake.inventoryCalls != 2 {
		t.Fatalf("settled idle host polled again: inventory calls=%d", fake.inventoryCalls)
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
	for _, mode := range []string{"pending-stop", "desk-stop", "direct-stop"} {
		t.Run(mode, func(t *testing.T) {
			a, r := executionTestApp(t)
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				switch mode {
				case "pending-stop":
					st.Starts[0].Execution.StopRequest = &execution.StopRequest{ID: "stop-missing", Reason: "pending stop fixture"}
				case "desk-stop":
					st.Stop = &state.Stop{Reason: "operator desk stop"}
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			fake := &orphanTransport{testExecutionTransport: &testExecutionTransport{
				sync: func(context.Context, execution.Ref) (execution.ControlResult, error) {
					return execution.ControlResult{}, execution.ErrNotFound
				},
				stop: func(context.Context, execution.Ref, execution.StopRequest) (execution.Ack, error) {
					return execution.Ack{}, execution.ErrNotFound
				},
				inspect: func(context.Context, execution.Ref) (execution.Observation, error) {
					return execution.Observation{}, execution.ErrNotFound
				},
			}, absent: true}
			a.executionTransport = func(execution.Host) execution.Transport { return fake }
			var err error
			if mode == "direct-stop" {
				err = a.stopExecution(context.Background(), r.Ref, "operator stop")
			} else {
				err = a.syncExecutionHost(context.Background(), sshTestSelector)
			}
			if err != nil {
				t.Fatalf("%s err=%v", mode, err)
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			rec := st.Starts[0].Execution
			if !rec.Terminal() || !rec.HostJournalMissing || rec.Observation.StopAcknowledged || executionNeedsSync(st, rec) {
				t.Fatalf("%s: lost execution with stop was not settled honestly: %+v", mode, rec)
			}
			unconfirmed, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "execution.unconfirmed" })
			if err != nil || len(unconfirmed) != 0 {
				t.Fatalf("%s recorded unconfirmed events: %+v %v", mode, unconfirmed, err)
			}
		})
	}
}

func TestPreInventoryHostUpgradeUnblocksMissingJournalReconciliation(t *testing.T) {
	a, r := executionTestApp(t)
	withCoordinatorVersion(t, hostNewRelease)
	upgraded := false
	fake := &orphanTransport{testExecutionTransport: &testExecutionTransport{
		sync: func(context.Context, execution.Ref) (execution.ControlResult, error) {
			return execution.ControlResult{}, execution.ErrNotFound
		},
		inspect: func(context.Context, execution.Ref) (execution.Observation, error) {
			return execution.Observation{}, execution.ErrNotFound
		},
	}, absent: true}
	a.executionTransport = func(execution.Host) execution.Transport {
		if !upgraded {
			return &unsupportedMissingTransport{testExecutionTransport: fake.testExecutionTransport}
		}
		return fake
	}
	digest := execution.Digest([]byte("upgraded host policy"))
	lookup := lookupRelease
	t.Cleanup(func() { lookupRelease = lookup })
	lookupRelease = func(_ context.Context, version string) (update.Release, error) {
		return update.Release{Tag: version}, nil
	}
	a.executionUpdate = func(context.Context, execution.Host, string, []byte) (execution.HostUpdate, error) {
		upgraded = true
		return execution.HostUpdate{Protocol: execution.Protocol, Identity: sshTestSelector, Version: hostNewRelease, PolicyDigest: digest}, nil
	}
	a.executionProbe = func(_ context.Context, h execution.Host) (execution.Capabilities, error) {
		return execution.Capabilities{CIWait: true, SourcePreparation: true, Publication: true, Protocol: execution.Protocol, Identity: h.Identity, Version: hostNewRelease, PolicyDigest: h.PolicyDigest, Harnesses: map[string]string{execution.HarnessClaude: hostFixtureClaudeVersion}, Resources: execution.Resources{ObservedAt: a.now, AvailableMiB: 1 << 16}}, nil
	}
	// Before the host upgrades, Missing returns ErrUnsupported so unit state cannot be confirmed yet.
	if err := a.syncExecutionHost(context.Background(), sshTestSelector); err == nil {
		t.Fatal("expected unconfirmed sync while pre-inventory host cannot confirm unit state")
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Starts[0].Execution.Terminal() || !st.Starts[0].Execution.HostJournalUnconfirmed {
		t.Fatalf("expected unconfirmed missing journal on pre-inventory host: %+v", st.Starts[0].Execution)
	}
	// Upgrading the host must not deadlock on the missing-journal execution and must reconcile it once upgraded.
	if _, err := a.updateExecutionHost(context.Background(), sshTestSelector); err != nil {
		t.Fatalf("host update deadlocked on missing-journal execution: %v", err)
	}
	st, err = a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	rec := st.Starts[0].Execution
	if !rec.Terminal() || !rec.HostJournalMissing || rec.HostJournalUnconfirmed || !rec.Settled || rec.Request.Ref != r.Ref {
		t.Fatalf("missing execution not settled after host upgrade: %+v", rec)
	}
}

type unsupportedMissingTransport struct {
	*testExecutionTransport
}

func (f *unsupportedMissingTransport) Inventory(context.Context, string) ([]execution.Ref, error) {
	return nil, execution.ErrUnsupported
}

func (f *unsupportedMissingTransport) Missing(context.Context, execution.Ref) (bool, error) {
	return false, execution.ErrUnsupported
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
	w := &Watcher{Host: a}
	w.SyncExecutions(context.Background(), st)
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
