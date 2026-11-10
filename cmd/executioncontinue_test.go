package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

// exitedExecutionApp is a worker whose unit exited after its review park, and
// a host that continues it and stores what reaches it.
func exitedExecutionApp(t *testing.T) (*app, execution.StartRequest, *testExecutionTransport, *int) {
	t.Helper()
	a, r := executionTestApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		x := st.Starts[0].Execution
		x.Request.Messaging, x.Completed, x.Phase, x.Settled, x.TranscriptCollected = true, true, execution.Exited, true, true
		x.Observation.ProcessState = execution.Exited
		st.Starts[0].Task = r.Task
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	continued := 0
	fake := &testExecutionTransport{
		resume: func(_ context.Context, ref execution.Ref, q execution.ContinueRequest) (execution.Handle, error) {
			continued++
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if x, _ := executionRecord(st, ref); x.Terminal() || x.Completed || x.Phase != execution.Activating {
				t.Fatalf("record not revived before the host call: %+v", x)
			}
			signing, err := execution.SigningKey(filepath.Join(a.cfg.StateDir, "execution-coordinator"))
			if err != nil {
				t.Fatal(err)
			}
			signed := r
			signed.Messaging, signed.RegistrationReceipt = true, q.Receipt
			if err := execution.Verify(signed, execution.PublicKey(signing), time.Now().UTC()); err != nil {
				t.Fatalf("receipt: %v", err)
			}
			return execution.Handle{Ref: ref}, nil
		},
		message: func(_ context.Context, _ execution.Ref, m execution.Envelope) (execution.Ack, error) {
			return execution.Ack{ID: m.ID, Sequence: m.Sequence, Stage: execution.Stored}, nil
		},
	}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	return a, r, fake, &continued
}

func TestParkedExecutionResumesAfterItsProcessExited(t *testing.T) {
	a, r, _, continued := exitedExecutionApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park = &state.Park{At: time.Now().UTC(), On: "#1", Waits: "the note answer"}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	by := state.Party{Session: sshMessageSenderSession, Name: sshMessageSender, Host: sshTestCoordinator}
	if err := a.resumeParkedOpts(context.Background(), by, r.Name, false); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	if *continued != 0 || len(st.Starts[0].Execution.Messages) != 0 || st.Agents[0].Park == nil || len(st.Agents[0].Park.Continuation) != 1 {
		t.Fatalf("old runner continued: %d; park %+v", *continued, st.Agents[0].Park)
	}

}

func TestEndedExecutionComesBackOnTheRosterByName(t *testing.T) {
	a, r, _, continued := exitedExecutionApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		removeAgent(st, 0)
		st.Records = nil
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	s, ok := a.endedWorker(st, r.Name)
	if !ok {
		t.Fatal("an SSH worker off the roster is not found by name")
	}
	err := a.wakeEnded(context.Background(), state.Party{Name: sshMessageSender}, s, "the review found this", "")
	var pending *executionDeliveryPending
	if !errors.As(err, &pending) {
		t.Fatalf("wake = %v", err)
	}
	st, _ = a.store.Read()
	if *continued != 1 || len(st.Agents) != 1 || st.Agents[0].Execution == nil || st.Agents[0].Task != r.Task {
		t.Fatalf("continued %d, roster %+v", *continued, st.Agents)
	}
}

func TestUnparkedExitedExecutionResumesByName(t *testing.T) {
	a, r, _, continued := exitedExecutionApp(t)
	by := state.Party{Session: sshMessageSenderSession, Name: sshMessageSender, Host: sshTestCoordinator}
	err := a.resumeParkedOpts(context.Background(), by, r.Name, false)
	var pending *executionDeliveryPending
	if !errors.As(err, &pending) {
		t.Fatalf("resume = %v", err)
	}
	st, _ := a.store.Read()
	x := st.Starts[0].Execution
	if *continued != 1 || len(x.Messages) != 1 || x.Request.ConversationID != r.ConversationID || *st.Agents[0].Execution != r.Ref {
		t.Fatalf("continued %d, execution %+v, agent %+v", *continued, x, st.Agents[0])
	}
}

func TestUnparkedExecutionResumeWaitsForItsLastTurn(t *testing.T) {
	a, r, fake, continued := exitedExecutionApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		x := st.Starts[0].Execution
		x.Phase, x.Observation.ProcessState = execution.Running, execution.Running
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		return execution.Observation{Handle: execution.Handle{Ref: r.Ref, ConversationID: r.ConversationID, Repository: r.Repository, Unit: execution.UnitName(r.Ref)}, ProcessState: execution.Running, Ready: true}, nil
	}
	err := a.resumeParkedOpts(context.Background(), state.Party{Name: sshMessageSender}, r.Name, false)
	if err == nil || !strings.Contains(err.Error(), "last turn") || *continued != 0 {
		t.Fatalf("resume = %v, continued %d", err, *continued)
	}
	st, _ := a.store.Read()
	if len(st.Starts[0].Execution.Messages) != 0 {
		t.Fatal("resume queued a message while the completed runner was still ending")
	}
}

func TestActiveUnparkedExecutionIsNotResumed(t *testing.T) {
	a, r, _, continued := exitedExecutionApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		x := st.Starts[0].Execution
		x.Completed, x.Phase, x.Observation.ProcessState = false, execution.Running, execution.Running
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	err := a.resumeParkedOpts(context.Background(), state.Party{Name: sshMessageSender}, r.Name, false)
	if err == nil || !strings.Contains(err.Error(), "is not parked") || *continued != 0 {
		t.Fatalf("active worker resume = %v, continued %d", err, *continued)
	}
}

func TestUnparkedRetiredExecutionResumeNamesItsTranscript(t *testing.T) {
	a, r, _, continued := exitedExecutionApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Retirement = &state.ExecutionRetirement{Trigger: execution.RetireTriggerReport}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	err := a.resumeParkedOpts(context.Background(), state.Party{Name: sshMessageSender}, r.Name, false)
	if !errors.Is(err, errGone) || !strings.Contains(err.Error(), "transcript") || *continued != 0 {
		t.Fatalf("resume = %v, continued %d", err, *continued)
	}
}

func TestEndedExecutionRefusalNamesWhyAndItsTranscript(t *testing.T) {
	for name, change := range map[string]func(*state.ExecutionRecord){
		"stopped": func(x *state.ExecutionRecord) {
			x.StopRequest = &execution.StopRequest{ID: "stop-1", Reason: "operator"}
			x.Observation.StopAcknowledged = true
		},
		"retired": func(x *state.ExecutionRecord) {
			x.Retirement = &state.ExecutionRetirement{Trigger: execution.RetireTriggerReport}
		},
		execution.HarnessOmp: func(x *state.ExecutionRecord) { x.Request.Harness = execution.HarnessOmp },
	} {
		t.Run(name, func(t *testing.T) {
			a, r, _, continued := exitedExecutionApp(t)
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				change(st.Starts[0].Execution)
				removeAgent(st, 0)
				st.Records = nil
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			st, _ := a.store.Read()
			s, ok := a.endedWorker(st, string(r.Ref.Execution))
			if !ok {
				t.Fatal("not found by execution identity")
			}
			err := a.wakeEnded(context.Background(), state.Party{Name: sshMessageSender}, s, "go on", "")
			if !errors.Is(err, errGone) || !strings.Contains(err.Error(), "transcript") {
				t.Fatalf("refusal = %v", err)
			}
			st, _ = a.store.Read()
			if *continued != 0 || len(st.Agents) != 0 {
				t.Fatalf("continued %d, roster %+v", *continued, st.Agents)
			}
		})
	}
}

func TestEndedExecutionIdentitySurvivesReusedWorkerName(t *testing.T) {
	a, r, _, continued := exitedExecutionApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		removeAgent(st, 0)
		st.Records = nil
		st.Starts = append(st.Starts, state.Start{
			Party:   state.Party{Name: r.Name, Session: offID},
			At:      time.Now().UTC().Add(time.Minute),
			Harness: execution.HarnessCodex,
		})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	s, ok := a.endedWorker(st, string(r.Ref.Execution))
	if !ok || s.Execution == nil || s.Execution.Request.Ref != r.Ref {
		t.Fatalf("execution identity resolved to another worker: %+v, %v", s, ok)
	}
	err := a.wakeEnded(context.Background(), state.Party{Name: sshMessageSender}, s, "finish the proof", "")
	var pending *executionDeliveryPending
	if !errors.As(err, &pending) {
		t.Fatalf("wake = %v", err)
	}
	st, _ = a.store.Read()
	if *continued != 1 || len(st.Agents) != 1 || st.Agents[0].Execution == nil || *st.Agents[0].Execution != r.Ref {
		t.Fatalf("continued %d, roster %+v", *continued, st.Agents)
	}
}

func TestHostRefusalLeavesTheExecutionAsItWas(t *testing.T) {
	a, _, fake, _ := exitedExecutionApp(t)
	fake.resume = func(context.Context, execution.Ref, execution.ContinueRequest) (execution.Handle, error) {
		return execution.Handle{}, errors.New("execution host has no room")
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Done, st.Agents[0].LastTask, st.Agents[0].Task = true, sshTestTask, ""
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	err := a.continueExecution(context.Background(), state.Party{Name: sshMessageSender}, st.Agents[0])
	if err == nil || !strings.Contains(err.Error(), "no room") {
		t.Fatalf("continue = %v", err)
	}
	st, _ = a.store.Read()
	x := st.Starts[0].Execution
	if !x.Terminal() || !x.Completed || x.Phase != execution.Exited || !x.Settled || !st.Agents[0].Done || st.Agents[0].Task != "" {
		t.Fatalf("record %+v, agent %+v", x, st.Agents[0])
	}
}

func TestOlderHostIsNamedForAnUpdate(t *testing.T) {
	a, _, fake, _ := exitedExecutionApp(t)
	fake.resume = func(context.Context, execution.Ref, execution.ContinueRequest) (execution.Handle, error) {
		return execution.Handle{}, execution.ErrUnsupported
	}
	st, _ := a.store.Read()
	err := a.continueExecution(context.Background(), state.Party{Name: sshMessageSender}, st.Agents[0])
	if err == nil || !strings.Contains(err.Error(), "update the host") {
		t.Fatalf("continue = %v", err)
	}
}

// A timer or settled wait for a worker that is done and not parked must not
// bring it back: only a wake by name does.
func TestDoneExecutionStaysEndedForOtherWakes(t *testing.T) {
	a, _, _, continued := exitedExecutionApp(t)
	st, _ := a.store.Read()
	err := a.enqueueExecutionWake(context.Background(), state.Party{Name: sshMessageSender}, st.Agents[0], "timer fired", nil)
	if err == nil || *continued != 0 {
		t.Fatalf("wake = %v, continued %d", err, *continued)
	}
}

func TestCompletedExecutionStillInItsLastTurnIsNotContinued(t *testing.T) {
	a, r, fake, continued := exitedExecutionApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		x := st.Starts[0].Execution
		x.Phase, x.Observation.ProcessState = execution.Running, execution.Running
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		return execution.Observation{Handle: execution.Handle{Ref: r.Ref, ConversationID: r.ConversationID, Repository: r.Repository, Unit: execution.UnitName(r.Ref)}, ProcessState: execution.Running, Ready: true}, nil
	}
	st, _ := a.store.Read()
	err := a.continueExecution(context.Background(), state.Party{Name: sshMessageSender}, st.Agents[0])
	if err == nil || !strings.Contains(err.Error(), "last turn") || *continued != 0 {
		t.Fatalf("continue = %v, continued %d", err, *continued)
	}
}
