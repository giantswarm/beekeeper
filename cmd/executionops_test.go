package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

const sshTestInspect = "inspect"

type testExecutionTransport struct {
	execution.Transport
	resolve func(context.Context, execution.Ref, execution.MessageResolution) (execution.Ack, error)
	message func(context.Context, execution.Ref, execution.Envelope) (execution.Ack, error)
	start   func(context.Context, execution.StartRequest) (execution.Handle, error)
	logs    func(context.Context, execution.Ref, execution.LogRequest) (execution.LogPage, error)
	stop    func(context.Context, execution.Ref, execution.StopRequest) (execution.Ack, error)
	inspect func(context.Context, execution.Ref) (execution.Observation, error)
	sync    func(context.Context, execution.Ref) (execution.ControlResult, error)
	rebind  func(context.Context, execution.Ref, execution.PolicyRebind) (execution.Observation, error)
}

func (f *testExecutionTransport) ResolveMessage(ctx context.Context, r execution.Ref, q execution.MessageResolution) (execution.Ack, error) {
	if f.resolve == nil {
		return execution.Ack{}, execution.ErrUnsupported
	}
	return f.resolve(ctx, r, q)
}

func (f *testExecutionTransport) Message(ctx context.Context, r execution.Ref, m execution.Envelope) (execution.Ack, error) {
	return f.message(ctx, r, m)
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
		if !ok || time.Until(deadline) <= 5*time.Second || time.Until(deadline) > 30*time.Second {
			t.Fatalf("watch SSH work deadline = %v (ok=%v), want >5s and <=30s", time.Until(deadline), ok)
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

func TestExecutionWatchDeadlineBackoffAndSingleUnconfirmedReport(t *testing.T) {
	a, r := executionTestApp(t)
	var out bytes.Buffer
	a.out = &out
	doneRef := execution.Ref{Host: sshTestSelector, Execution: "11111111-1111-1111-1111-111111111111", Generation: 1}
	doneHandle := execution.Handle{Ref: doneRef, ConversationID: string(doneRef.Execution), Repository: r.Repository, Unit: execution.UnitName(doneRef)}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append([]state.Start{{
			Party: state.Party{Session: string(doneRef.Execution), Name: r.Name, Host: string(doneRef.Host)},
			Execution: &state.ExecutionRecord{
				Completed:   true,
				Handle:      doneHandle,
				Request:     r,
				Selector:    sshTestSelector,
				Phase:       execution.Exited,
				Observation: execution.Observation{Handle: doneHandle, ProcessState: execution.Exited, Reachability: execution.Reachable, Ready: true},
			},
		}}, st.Starts...)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	handle := st.Starts[1].Execution.Handle
	failDeadline := true
	syncCalls, inspectCalls, doneCalls := 0, 0, 0
	fake := &testExecutionTransport{
		sync: func(_ context.Context, ref execution.Ref) (execution.ControlResult, error) {
			if ref == doneRef {
				doneCalls++
				return execution.ControlResult{}, nil
			}
			syncCalls++
			if failDeadline {
				return execution.ControlResult{}, &execution.UnreachableError{Operation: "sync-control", Cause: context.DeadlineExceeded}
			}
			return execution.ControlResult{}, nil
		},
		inspect: func(_ context.Context, ref execution.Ref) (execution.Observation, error) {
			if ref == doneRef {
				doneCalls++
			} else {
				inspectCalls++
			}
			if failDeadline {
				return execution.Observation{}, &execution.UnreachableError{Operation: sshTestInspect, Cause: context.DeadlineExceeded}
			}
			return execution.Observation{Handle: handle, ProcessState: execution.Running, Ready: true}, nil
		},
	}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	w := &watcher{app: a}

	// First tick hits the deadline: reported once as deadline exceeded, not
	// unreachable, and does not keep calling inspect on the timed-out host or
	// reconciling already-settled terminal executions.
	st = w.syncExecutions(context.Background(), st)
	if doneCalls != 0 {
		t.Fatalf("settled terminal execution was reconciled %d times", doneCalls)
	}
	if syncCalls != 1 || inspectCalls != 0 {
		t.Fatalf("first deadline pass calls: sync=%d inspect=%d, want sync=1 inspect=0", syncCalls, inspectCalls)
	}
	if got := out.String(); !strings.Contains(got, "EXECUTION UNCONFIRMED: "+sshTestSelector+":") || !strings.Contains(got, "deadline exceeded") || strings.Contains(got, "unreachable") {
		t.Fatalf("unexpected watch output on deadline:\n%s", got)
	}

	// Next minute is inside failure backoff: no retry, no ENDED flap.
	w.now = w.now.Add(time.Minute)
	st = w.syncExecutions(context.Background(), st)
	if syncCalls != 1 {
		t.Fatalf("reconciled during 2m backoff at +1m: syncCalls=%d", syncCalls)
	}

	// At +2m, the first backoff retry runs and still hits the deadline: no
	// duplicate EXECUTION UNCONFIRMED or ENDED line, and next backoff is 4m.
	w.now = w.now.Add(time.Minute)
	st = w.syncExecutions(context.Background(), st)
	if syncCalls != 2 {
		t.Fatalf("did not retry at +2m backoff: syncCalls=%d, want 2", syncCalls)
	}
	w.now = w.now.Add(2 * time.Minute)
	st = w.syncExecutions(context.Background(), st)
	if syncCalls != 2 {
		t.Fatalf("reconciled inside 4m second backoff: syncCalls=%d", syncCalls)
	}

	events, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "execution.unconfirmed" })
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("execution.unconfirmed events = %d, want 1 while staying unconfirmed", len(events))
	}
	if strings.Count(out.String(), "EXECUTION UNCONFIRMED") != 1 || strings.Contains(out.String(), "ENDED EXECUTION UNCONFIRMED") {
		t.Fatalf("watch flapped while execution stayed unconfirmed:\n%s", out.String())
	}

	// At +4m after the second failure, the host recovers and ENDED is emitted once.
	failDeadline = false
	w.now = w.now.Add(2 * time.Minute)
	st = w.syncExecutions(context.Background(), st)
	if syncCalls != 3 || inspectCalls != 1 || st.Starts[1].Execution.Observation.Reachability != execution.Reachable {
		t.Fatalf("recovery after backoff failed: sync=%d inspect=%d reachability=%s", syncCalls, inspectCalls, st.Starts[1].Execution.Observation.Reachability)
	}
	if strings.Count(out.String(), "ENDED EXECUTION UNCONFIRMED") != 1 {
		t.Fatalf("expected one ENDED EXECUTION UNCONFIRMED after recovery:\n%s", out.String())
	}
}

func TestExecutionWatchEndedRunSettlesAndMultiHostRotationDoesNotFlap(t *testing.T) {
	a, r := executionTestApp(t)
	var out bytes.Buffer
	a.out = &out
	const hostB = "host-b"
	refB := execution.Ref{Host: hostB, Execution: "22222222-2222-2222-2222-222222222222", Generation: 1}
	handleB := execution.Handle{Ref: refB, ConversationID: string(refB.Execution), Repository: r.Repository, Unit: execution.UnitName(refB)}
	reqB := r
	reqB.Ref = refB
	reqB.ConversationID = string(refB.Execution)
	reqB.Name = "parked-worker-b"
	hA := a.cfg.Agents.Hosts[sshTestSelector]
	hB := hA
	hB.Identity = hostB
	hB.SSH = "host-b.example"
	a.cfg.Agents.Hosts[hostB] = hB
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		partyB := state.Party{Session: reqB.ConversationID, Name: reqB.Name, Host: hostB}
		st.Agents = append(st.Agents, state.Agent{Party: partyB, Task: r.Task, Execution: &refB, Park: &state.Park{By: partyB, At: a.now, Waits: "proof timer"}})
		st.Starts = append(st.Starts, state.Start{
			Party: partyB,
			Execution: &state.ExecutionRecord{
				Handle:        handleB,
				Request:       reqB,
				RequestDigest: reqB.Digest(),
				Selector:      hostB,
				Phase:         execution.Running,
				Observation:   execution.Observation{Handle: handleB, ProcessState: execution.Running, Reachability: execution.Reachable, Ready: true},
			},
		})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	handleA := st.Starts[0].Execution.Handle
	callsA, callsB := 0, 0
	fakeA := &testExecutionTransport{
		sync: func(context.Context, execution.Ref) (execution.ControlResult, error) {
			callsA++
			return execution.ControlResult{}, nil
		},
		inspect: func(context.Context, execution.Ref) (execution.Observation, error) {
			callsA++
			// Worker A's proof run ended; its process is now Exited.
			return execution.Observation{Handle: handleA, ProcessState: execution.Exited, Ready: true}, nil
		},
	}
	fakeB := &testExecutionTransport{
		sync: func(context.Context, execution.Ref) (execution.ControlResult, error) {
			callsB++
			return execution.ControlResult{}, &execution.UnreachableError{Operation: "sync-control", Cause: context.DeadlineExceeded}
		},
		inspect: func(context.Context, execution.Ref) (execution.Observation, error) {
			callsB++
			return execution.Observation{Handle: handleB, ProcessState: execution.Running, Ready: true}, nil
		},
	}
	a.executionTransport = func(h execution.Host) execution.Transport {
		if h.Identity == hostB {
			return fakeB
		}
		return fakeA
	}
	w := &watcher{app: a}

	// Minute 0: host-a observes its ended run (sync + inspect + final post-exit sync)
	// and settles; host-b hits its deadline and emits EXECUTION UNCONFIRMED once.
	st = w.syncExecutions(context.Background(), st)
	if callsA != 3 || !st.Starts[0].Execution.Settled {
		t.Fatalf("ended run on host-a did not settle after final sync: callsA=%d settled=%v", callsA, st.Starts[0].Execution.Settled)
	}
	if callsB != 1 {
		t.Fatalf("host-b calls on minute 0 = %d, want 1", callsB)
	}

	// Minutes 1, 2, 3: rotation changes host order, but settled host-a is never
	// polled again and unconfirmed host-b stays unconfirmed without ENDED flaps.
	for range 3 {
		w.now = w.now.Add(time.Minute)
		st = w.syncExecutions(context.Background(), st)
	}
	if callsA != 3 {
		t.Fatalf("settled ended execution on host-a was polled again: callsA=%d", callsA)
	}
	if callsB != 2 {
		t.Fatalf("host-b should retry once at +2m during 3 minutes: callsB=%d, want 2", callsB)
	}
	if strings.Count(out.String(), "EXECUTION UNCONFIRMED") != 1 || strings.Contains(out.String(), "ENDED EXECUTION UNCONFIRMED") {
		t.Fatalf("multi-host rotation flapped UNCONFIRMED/ENDED:\n%s", out.String())
	}
}

func (f *testExecutionTransport) Start(ctx context.Context, r execution.StartRequest) (execution.Handle, error) {
	return f.start(ctx, r)
}

func (f *testExecutionTransport) Logs(ctx context.Context, ref execution.Ref, q execution.LogRequest) (execution.LogPage, error) {
	return f.logs(ctx, ref, q)
}

func (f *testExecutionTransport) Stop(ctx context.Context, r execution.Ref, q execution.StopRequest) (execution.Ack, error) {
	return f.stop(ctx, r, q)
}
func (f *testExecutionTransport) Inspect(ctx context.Context, r execution.Ref) (execution.Observation, error) {
	return f.inspect(ctx, r)
}
func (f *testExecutionTransport) RebindPolicy(ctx context.Context, r execution.Ref, q execution.PolicyRebind) (execution.Observation, error) {
	if f.rebind == nil {
		return execution.Observation{}, errors.New("fixture transport has no policy rebind")
	}
	return f.rebind(ctx, r, q)
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
		return execution.Observation{}, &execution.UnreachableError{Operation: sshTestInspect, Cause: errors.New("fixture disconnection")}
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

func TestExecutionLostLogsAndInspectExposeReachabilityWithoutChangingTask(t *testing.T) {
	a, r := executionTestApp(t)
	a.out = &bytes.Buffer{}
	fake := &testExecutionTransport{logs: func(context.Context, execution.Ref, execution.LogRequest) (execution.LogPage, error) {
		return execution.LogPage{}, &execution.UnreachableError{Operation: "logs", Cause: errors.New("fixture disconnection")}
	}, inspect: func(context.Context, execution.Ref) (execution.Observation, error) {
		return execution.Observation{}, &execution.UnreachableError{Operation: sshTestInspect, Cause: errors.New("fixture disconnection")}
	}}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	if _, err := runAgents(a, "logs", r.Name); err == nil {
		t.Fatal("lost logs reported success")
	}
	out, err := runAgents(a, sshTestInspect, r.Name)
	if err == nil || !strings.Contains(out, `"reachability": "unreachable"`) || !strings.Contains(out, `"processState": "running"`) {
		t.Fatalf("unreachable inspection: %s, %v", out, err)
	}
	st, err := a.store.Read()
	if err != nil || st.Agents[0].Task != r.Task || st.Starts[0].Execution.Terminal() {
		t.Fatal("read failure changed task or process ownership")
	}
}

// After a host upgrade re-prepares its policy under a running execution,
// recover tells a reachable unit from one whose publication stays unavailable,
// and rebinds it only to the policy the host catalog approved, once, with an
// audit event and no replacement start.
func TestExecutionRecoverRebindsApprovedHostPolicy(t *testing.T) {
	a, r := executionTestApp(t)
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	handle := st.Starts[0].Execution.Handle
	original, prepared := r.PolicyDigest, execution.Digest([]byte("upgraded fixture policy"))
	bound, installed := original, ""
	var rebinds []execution.PolicyRebind
	fake := &testExecutionTransport{}
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		return execution.Observation{Handle: handle, ProcessState: execution.Running, Reachability: execution.Reachable, Ready: true, PolicyDigest: bound, HostPolicyDigest: installed}, nil
	}
	fake.start = func(context.Context, execution.StartRequest) (execution.Handle, error) {
		t.Fatal("recovery started a unit")
		return execution.Handle{}, nil
	}
	fake.rebind = func(ctx context.Context, ref execution.Ref, q execution.PolicyRebind) (execution.Observation, error) {
		rebinds = append(rebinds, q)
		bound = q.To
		return fake.inspect(ctx, ref)
	}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	ctx := context.Background()

	for _, step := range []struct{ installed, want string }{
		{"", "unprepared or stale"},
		{prepared, "not the catalog's approved"},
	} {
		installed = step.installed
		err := a.activateExecution(ctx, r.Ref)
		var refusal *exitError
		if !errors.As(err, &refusal) || refusal.code != ExitRefused || !strings.Contains(err.Error(), "is reachable") || !strings.Contains(err.Error(), "publication, merge and capped runs are unavailable") || !strings.Contains(err.Error(), step.want) {
			t.Fatalf("recover did not report the unavailable publication (%s): %v", step.want, err)
		}
	}
	if len(rebinds) != 0 {
		t.Fatal("recover rebound an unapproved policy")
	}

	host := a.cfg.Agents.Hosts[sshTestSelector]
	host.PolicyDigest = prepared
	a.cfg.Agents.Hosts[sshTestSelector] = host
	if err := a.activateExecution(ctx, r.Ref); err != nil {
		t.Fatal(err)
	}
	if len(rebinds) != 1 || rebinds[0] != (execution.PolicyRebind{From: original, To: prepared}) {
		t.Fatalf("recover sent the wrong rebind: %+v", rebinds)
	}
	if err := a.activateExecution(ctx, r.Ref); err != nil || len(rebinds) != 1 {
		t.Fatal("a recovery of a rebound execution rebound it again", err)
	}
	st, err = a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if o := st.Starts[0].Execution.Observation; o.PolicyDigest != prepared || !o.PolicyAvailable() || len(st.Starts) != 1 || len(st.Agents) != 1 {
		t.Fatalf("recorded execution does not show the rebound policy: %+v", o)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "execution.policy-rebound" })
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, original+" -> "+prepared) {
		t.Fatal("rebind left no audit event", evs, err)
	}
}
