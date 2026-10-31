package cmd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

const sshMessageSender = "sender"

func TestExecutionMessagesRetainParkUntilHarnessAcceptance(t *testing.T) {
	a, r := executionTestApp(t)
	p := &state.Park{At: time.Now().UTC(), On: "#1", Waits: "the note answer"}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Request.Messaging = true
		st.Agents[0].Park = p
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	by := state.Party{Session: "sender-session", Name: sshMessageSender, Host: sshTestCoordinator}
	stage := execution.Stored
	calls := 0
	fake := &testExecutionTransport{message: func(_ context.Context, ref execution.Ref, m execution.Envelope) (execution.Ack, error) {
		calls++
		st, _ := a.store.Read()
		if len(st.Starts[0].Execution.Messages) != 1 {
			t.Fatal("message sent before durable outbox write")
		}
		if ref != r.Ref || m.To != r.Ref || m.From != r.Ref || m.Sender.Session != by.Session || m.Sender.Host != by.Host {
			t.Fatal("sender or worker identity changed")
		}
		return execution.Ack{ID: m.ID, Sequence: m.Sequence, Stage: stage}, nil
	}}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	st, _ := a.store.Read()
	err := a.resumeParkedOpts(context.Background(), by, st.Agents[0].Name, false)
	if err == nil {
		t.Fatal("storage counted as harness acceptance")
	}
	st, _ = a.store.Read()
	if st.Agents[0].Park == nil {
		t.Fatal("park cleared on storage only")
	}
	// Restart the coordinating app and lose its process-local state.
	restarted := *a
	stage = execution.Accepted
	if err := restarted.syncExecutionMessages(context.Background(), r.Ref); err != nil {
		t.Fatal(err)
	}
	st, _ = a.store.Read()
	if st.Agents[0].Park != nil || st.Starts[0].Execution.Messages[0].Stage != execution.Accepted {
		t.Fatal("accepted resume did not clear its original park")
	}
	if err := a.syncExecutionMessages(context.Background(), r.Ref); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("acknowledged envelope resent")
	}
}

func TestExecutionMessagesRecoverLostReplyAndRejectConflictingKey(t *testing.T) {
	a, r := executionTestApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Request.Messaging = true
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	ledger := execution.MessageLedger{}
	drop := true
	fake := &testExecutionTransport{message: func(_ context.Context, ref execution.Ref, m execution.Envelope) (execution.Ack, error) {
		ack, err := ledger.Store(ref, m)
		if err != nil {
			return ack, err
		}
		if ack.Stage != execution.Accepted {
			if err := ledger.Attempt(m); err != nil {
				t.Fatal(err)
			}
			if err := ledger.Accept(m.ID); err != nil {
				t.Fatal(err)
			}
		}
		if drop {
			return execution.Ack{}, &execution.UnreachableError{Operation: "message", Cause: errors.New("reply lost after acceptance")}
		}
		return execution.Ack{ID: m.ID, Sequence: m.Sequence, Stage: execution.Accepted}, nil
	}}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	a.executionRequestID = "one-request"
	st, _ := a.store.Read()
	by := state.Party{Name: sshMessageSender}
	if err := a.enqueueExecutionWake(context.Background(), by, st.Agents[0], "continue", nil); err == nil {
		t.Fatal("lost reply reported delivery")
	}
	drop = false
	if err := a.enqueueExecutionWake(context.Background(), by, st.Agents[0], "continue", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.enqueueExecutionWake(context.Background(), by, st.Agents[0], "changed", nil); err == nil {
		t.Fatal("conflicting request admitted")
	}
	st, _ = a.store.Read()
	if len(st.Starts[0].Execution.Messages) != 1 || ledger.Sequence != 1 {
		t.Fatal("reconnect duplicated the accepted message")
	}
	if st.Starts[0].Execution.Handle.Ref != r.Ref {
		t.Fatal("reconnect replaced execution")
	}
}

func TestExecutionWakeNoteAnswerAndTimerShareDurableDelivery(t *testing.T) {
	a, r := executionTestApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Request.Messaging = true
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	fake := &testExecutionTransport{message: func(_ context.Context, ref execution.Ref, m execution.Envelope) (execution.Ack, error) {
		return execution.Ack{ID: m.ID, Sequence: m.Sequence, Stage: execution.Accepted}, nil
	}}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	ctx := context.Background()
	by := state.Party{Name: sshMessageSender}
	if err := a.wakeAgent(ctx, by, r.Name, "ordinary wake", ""); err != nil {
		t.Fatal(err)
	}
	n := &state.Note{ID: 1, By: state.Party{Session: r.ConversationID, Name: r.Name}}
	a.tellFiler(ctx, by, n, "the note answer")
	w := &watcher{app: a}
	w.wakeTimer(ctx, timerFire{t: state.Timer{ID: 1, Wake: r.Name, What: "the timer"}, reason: "due"})
	st, _ := a.store.Read()
	if len(st.Starts[0].Execution.Messages) != 3 {
		t.Fatal("a wake source bypassed the shared outbox")
	}
	for i, m := range st.Starts[0].Execution.Messages {
		if m.Envelope.Sequence != uint64(i+1) || m.Stage != execution.Accepted {
			t.Fatal("wake sources lost ordered acceptance")
		}
	}
}
