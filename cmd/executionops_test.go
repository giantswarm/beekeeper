package cmd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

type testExecutionTransport struct {
	execution.Transport
	stop    func(context.Context, execution.Ref, execution.StopRequest) (execution.Ack, error)
	inspect func(context.Context, execution.Ref) (execution.Observation, error)
	sync    func(context.Context, execution.Ref) (execution.ControlResult, error)
}

func TestExecutionWatchReconcilesWithBoundedDeadlineAndThrottle(t *testing.T) {
	a, r := executionTestApp(t)
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	handle := st.Starts[0].Execution.Handle
	calls := 0
	fake := &testExecutionTransport{sync: func(ctx context.Context, ref execution.Ref) (execution.ControlResult, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("watch SSH work has no bounded deadline")
		}
		if ref != r.Ref {
			t.Fatal("watch adopted another execution")
		}
		return execution.ControlResult{}, nil
	}, inspect: func(context.Context, execution.Ref) (execution.Observation, error) {
		return execution.Observation{Handle: handle, ProcessState: execution.Running, Ready: true}, nil
	}}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	w := &watcher{app: a}
	st = w.syncExecutions(context.Background(), st)
	if calls != 1 || st.Starts[0].Execution.Observation.Reachability != execution.Reachable {
		t.Fatal("watch did not reconcile the owned execution")
	}
	w.syncExecutions(context.Background(), st)
	if calls != 1 {
		t.Fatal("watch reconciled again inside its interval")
	}
	w.now = w.now.Add(time.Minute)
	w.syncExecutions(context.Background(), st)
	if calls != 2 {
		t.Fatal("watch failed to reconcile after its interval")
	}
}

func (f *testExecutionTransport) Stop(ctx context.Context, r execution.Ref, q execution.StopRequest) (execution.Ack, error) {
	return f.stop(ctx, r, q)
}
func (f *testExecutionTransport) Inspect(ctx context.Context, r execution.Ref) (execution.Observation, error) {
	return f.inspect(ctx, r)
}
func (f *testExecutionTransport) SyncControl(ctx context.Context, r execution.Ref) (execution.ControlResult, error) {
	if f.sync != nil {
		return f.sync(ctx, r)
	}
	return execution.ControlResult{}, nil
}

func TestExecutionStopPersistsIntentBeforeSSHAndKeepsTask(t *testing.T) {
	a, r := executionTestApp(t)
	calls := 0
	fake := &testExecutionTransport{}
	fake.stop = func(_ context.Context, ref execution.Ref, q execution.StopRequest) (execution.Ack, error) {
		calls++
		st, err := a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		record, _ := executionRecord(st, ref)
		if record == nil || record.StopRequest == nil || record.StopRequest.ID != q.ID {
			t.Fatal("SSH stop preceded durable coordinator intent")
		}
		if calls == 1 {
			return execution.Ack{}, &execution.UnreachableError{Operation: "stop", Cause: errors.New("fixture disconnection")}
		}
		return execution.Ack{ID: q.ID, Stage: execution.Applied}, nil
	}
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		return execution.Observation{}, &execution.UnreachableError{Operation: "inspect", Cause: errors.New("fixture disconnection")}
	}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	if err := a.stopExecution(context.Background(), r.Ref, "fixture stop"); err == nil {
		t.Fatal("unreachable stop reported success")
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	record, _ := executionRecord(st, r.Ref)
	if st.Agents[0].Task != sshTestTask || record.Terminal() || record.Observation.Reachability != execution.Unreachable || record.Observation.ProcessState != execution.Running {
		t.Fatal("transport loss changed task or inferred termination")
	}
	requestID := record.StopRequest.ID
	if err := a.stopExecution(context.Background(), r.Ref, "fixture retry"); err == nil {
		t.Fatal("lost inspection after stop acknowledgement was hidden")
	}
	st, err = a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	record, _ = executionRecord(st, r.Ref)
	if !record.Observation.StopAcknowledged || !record.Terminal() || record.StopRequest.ID != requestID || st.Agents[0].Task != sshTestTask {
		t.Fatal("confirmed stop was lost, duplicated or cleared its task")
	}
	if calls != 2 {
		t.Fatal("unexpected stop deliveries")
	}
}

func TestExecutionSyncRetriesOwnedStopAndIgnoresLocalEntries(t *testing.T) {
	a, r := executionTestApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: "personal-session"}, Task: "personal task"})
		st.Starts[0].Execution.StopRequest = &execution.StopRequest{ID: "fixture-stop", Reason: "fixture stop"}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	handle := st.Starts[0].Execution.Handle
	syncs, stops := 0, 0
	fake := &testExecutionTransport{sync: func(_ context.Context, ref execution.Ref) (execution.ControlResult, error) {
		if ref != r.Ref {
			t.Fatal("outbox reconciliation selected an unowned execution")
		}
		syncs++
		return execution.ControlResult{}, nil
	}, stop: func(_ context.Context, ref execution.Ref, q execution.StopRequest) (execution.Ack, error) {
		if ref != r.Ref {
			t.Fatal("stop selected an unowned execution")
		}
		stops++
		return execution.Ack{ID: q.ID, Stage: execution.Applied}, nil
	}, inspect: func(context.Context, execution.Ref) (execution.Observation, error) {
		return execution.Observation{Handle: handle, ProcessState: execution.Exited, StopRequested: true, StopAcknowledged: true}, nil
	}}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	if err := a.syncExecutionHost(context.Background(), sshTestSelector); err != nil {
		t.Fatal(err)
	}
	if syncs != 1 || stops != 1 {
		t.Fatal("owned stop or outbox was not reconciled exactly once")
	}
	st, err = a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Agents[1].Task != "personal task" || st.Agents[0].Task != sshTestTask {
		t.Fatal("reconciliation changed a task")
	}
}

func TestExecutionObservationCannotChangeOwnershipOrRevokeStopEvidence(t *testing.T) {
	a, r := executionTestApp(t)
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	handle := st.Starts[0].Execution.Handle
	wrong := execution.Observation{Handle: handle, ProcessState: execution.Running}
	wrong.Handle.Unit = "personal.service"
	if err := a.saveExecutionObservation(r.Ref, wrong); err == nil {
		t.Fatal("observation adopted another process handle")
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Observation.StopAcknowledged = true
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.saveExecutionObservation(r.Ref, execution.Observation{Handle: handle, ProcessState: execution.Running}); err != nil {
		t.Fatal(err)
	}
	st, err = a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Starts[0].Execution.Observation.StopAcknowledged || st.Starts[0].Execution.Observation.ProcessState != execution.Exited {
		t.Fatal("an older observation revoked acknowledged termination")
	}
}
