package execution

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const messageTestLinux = "linux"

func TestSSHMessagePersistsBeforeAcknowledgement(t *testing.T) {
	f := testFixture(t)
	// Keep this regression runnable against the pre-messaging base: old
	// StartRequest decoding ignores the added capability and rejects Message.
	var requestFields map[string]any
	_ = json.Unmarshal(mustJSON(f.r), &requestFields)
	requestFields["messaging"] = true
	if err := json.Unmarshal(mustJSON(requestFields), &f.r); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Start(context.Background(), f.r); err != nil {
		t.Fatal(err)
	}
	m := Envelope{ID: newID(), Sequence: 1, From: f.r.Ref, To: f.r.Ref, Kind: "wake", Text: "continue the fixture task"}
	payload, _ := json.Marshal(m)
	request, _ := json.Marshal(WireRequest{Protocol: Protocol, RequestID: "request-one", Operation: "message", Ref: f.r.Ref, Payload: payload})
	var output bytes.Buffer
	if err := f.s.Serve(context.Background(), bytes.NewReader(request), &output); err != nil {
		t.Fatal(err)
	}
	var reply WireResponse
	if err := json.Unmarshal(output.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Error != "" {
		t.Fatalf("owned message was refused instead of durably stored: %s", reply.Error)
	}
	var ack Ack
	if err := json.Unmarshal(reply.Payload, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.ID != m.ID || ack.Sequence != m.Sequence || ack.Stage != "stored" {
		t.Fatalf("storage acknowledgement missing: %+v", ack)
	}
}

func TestSSHMessageGapsDuplicatesAndInterruptedAcceptance(t *testing.T) {
	f := testFixture(t)
	f.r.Messaging = true
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	one := Envelope{ID: newID(), Sequence: 1, From: f.r.Ref, To: f.r.Ref, Kind: MessageWake, Text: "first message"}
	two := one
	two.ID = newID()
	two.Sequence = 2
	two.Text = "second message"
	if _, err := f.s.Message(ctx, f.r.Ref, two); err != nil {
		t.Fatal(err)
	}
	j, _ := f.s.Read(f.r.Ref)
	if _, ok := j.Messages.Next(); ok {
		t.Fatal("sequence gap delivered")
	}
	if _, err := f.s.Message(ctx, f.r.Ref, one); err != nil {
		t.Fatal(err)
	}
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error { return j.Messages.Attempt(one) }); err != nil {
		t.Fatal(err)
	}
	// Reopening the server loses all in-memory state. An attempted write is
	// uncertain, so replaying storage cannot send it to the harness again.
	restarted := *f.s
	ack, err := restarted.Message(ctx, f.r.Ref, one)
	if err != nil || ack.Stage != Uncertain {
		t.Fatalf("uncertain write replayed: %+v %v", ack, err)
	}
	j, _ = restarted.Read(f.r.Ref)
	if _, ok := j.Messages.Next(); ok {
		t.Fatal("uncertain message retried or overtaken")
	}
	conflict := one
	conflict.Text = "changed"
	if _, err := restarted.Message(ctx, f.r.Ref, conflict); err == nil {
		t.Fatal("conflicting duplicate admitted")
	}
	wrong := one
	wrong.To.Generation++
	if _, err := restarted.Message(ctx, f.r.Ref, wrong); err == nil {
		t.Fatal("wrong generation admitted")
	}
	// Only ID-bearing replay evidence counts. Matching text, init or result
	// events and another conversation's echo do not establish acceptance.
	for _, event := range []string{`{"type":"result"}`, `{"type":"system","subtype":"init"}`, `{"type":"user","message":{"content":"first message"}}`} {
		if acceptedMessage([]byte(event), f.r.ConversationID, one.ID) {
			t.Fatal("false harness acceptance")
		}
	}
	// Crash after the durable evidence but before the receipt transaction.
	evidence := struct{ ID, Session string }{one.ID, f.r.ConversationID}
	path := filepath.Join(f.s.Root, string(f.r.Ref.Execution)+"-"+one.ID+".accepted")
	if err := atomicFile(path, mustJSON(evidence)); err != nil {
		t.Fatal(err)
	}
	ack, err = restarted.Message(ctx, f.r.Ref, one)
	if err != nil || ack.Stage != Accepted {
		t.Fatalf("lost acceptance not reconciled: %+v %v", ack, err)
	}
	j, _ = restarted.Read(f.r.Ref)
	if next, ok := j.Messages.Next(); !ok || next.ID != two.ID {
		t.Fatal("ordered cursor did not advance exactly once")
	}
	if len(f.launcher.starts) != 0 {
		t.Fatal("message recovery launched a process")
	}
	for _, d := range j.Messages.Status() {
		if d.Envelope.Text != "" {
			t.Fatal("status exposed message text")
		}
	}
}

func TestSSHStreamingMessagesUseOneOwnedProcess(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("remote execution hosts require Linux")
	}
	f := testFixture(t)
	f.r.Messaging = true
	child := filepath.Join(t.TempDir(), "fake-harness")
	script := "#!/bin/sh\nexport BEEKEEPER_FAKE_STREAM=1\nexec " + quote(os.Args[0]) + " -test.run=^TestSSHStreamingHarnessHelper$\n"
	if err := os.WriteFile(child, []byte(script), 0o700); err != nil { //nolint:gosec // executable fake harness in its owned temporary directory
		t.Fatal(err)
	}
	h := f.s.Config.Harnesses[HarnessClaude]
	h.Executable = child
	f.s.Config.Harnesses[HarnessClaude] = h
	original := f.s.Command
	f.s.Command = func(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
		if bin == child {
			bin = fixtureClaudeExecutable
		}
		return original(ctx, dir, env, bin, args...)
	}
	digest, err := f.s.Prepare(f.s.Policy())
	if err != nil {
		t.Fatal(err)
	}
	f.r.PolicyDigest = digest
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.s.Run(ctx, f.r.Ref) }()
	one := Envelope{ID: newID(), Sequence: 1, From: f.r.Ref, To: f.r.Ref, Kind: MessageWake, Text: "first follow-up"}
	two := one
	two.ID = newID()
	two.Sequence = 2
	two.Text = "second follow-up"
	// Queue the gap first, including before the stream has bound its socket.
	if _, err := f.s.Message(ctx, f.r.Ref, two); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Message(ctx, f.r.Ref, one); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("fake harness did not receive its ordered messages")
	}
	for _, m := range []Envelope{one, two} {
		ack, err := f.s.Message(ctx, f.r.Ref, m)
		if err != nil || ack.Stage != Accepted {
			t.Fatalf("acknowledged message resent: %+v %v", ack, err)
		}
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if !j.Ready || j.Messages.Sequence != 2 || len(f.launcher.starts) != 1 {
		t.Fatal("duplicate process or missing conversation acceptance")
	}
	if err := f.s.Run(ctx, f.r.Ref); err == nil {
		t.Fatal("reconnection restarted a harness")
	}
}

func TestSSHStreamingHarnessHelper(t *testing.T) {
	if os.Getenv("BEEKEEPER_FAKE_STREAM") != "1" {
		return
	}
	if os.Getenv(EnvCoordinator) == "" || os.Getenv("BEEKEEPER_CONFIG") == "" {
		t.Fatal("worker identity lost")
	}
	scan := bufio.NewScanner(os.Stdin)
	count := 0
	for scan.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(scan.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		message, _ := ev["message"].(map[string]any)
		text, _ := message["content"].(string)
		if count == 1 && !strings.Contains(text, "first follow-up") || count == 2 && !strings.Contains(text, "second follow-up") {
			t.Fatal("message order changed")
		}
		ev["isReplay"] = true
		b, _ := json.Marshal(ev)
		if _, err := os.Stdout.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
		count++
		if count == 3 {
			return
		}
	}
	if count != 3 {
		t.Fatal("messages missing")
	}
}

func TestSSHCompletedReportClosesOwnedStream(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("remote execution hosts require Linux")
	}
	f := testFixture(t)
	f.r.Messaging = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	stream, err := f.s.openMessageStream(ctx, f.r.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	stream.Start(f.r)
	reader := bufio.NewReader(stream.reader)
	if _, err := reader.ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	want := ControlRequest{Operation: ControlReport, Data: json.RawMessage(`{"finished":true}`)}
	if err := f.s.finishControl(ctx, f.r.Ref, want, ControlResult{}); err != nil {
		t.Fatal(err)
	}
	_ = stream.reader.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatalf("completed worker left its harness input open: %v", err)
	}
}

func TestSSHMessageAuthorityLossKeepsTheExistingProcessAndStoredEnvelope(t *testing.T) {
	f := testFixture(t)
	f.r.Messaging = true
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	m := Envelope{ID: newID(), Sequence: 1, From: f.r.Ref, To: f.r.Ref, Kind: MessageWake, Text: "continue"}
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error { j.Ready = true; _, err := j.Messages.Store(f.r.Ref, m); return err }); err != nil {
		t.Fatal(err)
	}
	f.s.Authorize = func(context.Context, StartRequest) error { return errors.New("coordinator unreachable") }
	stream := &messageStream{s: f.s, ref: f.r.Ref, ctx: ctx}
	if err := stream.sendNext(); err != nil {
		t.Fatal(err)
	}
	j, _ := f.s.Read(f.r.Ref)
	if j.Messages.Entries[m.ID].Stage != Stored || len(f.launcher.starts) != 0 || len(f.launcher.stops) != 0 {
		t.Fatal("authority loss sent a message or changed a process")
	}
}

func TestSSHStoredMessageFailsOnlyOnConfirmedRunnerExit(t *testing.T) {
	f := testFixture(t)
	f.r.Messaging = true
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	m := Envelope{ID: newID(), Sequence: 1, From: f.r.Ref, To: f.r.Ref, Kind: MessageWake, Text: "continue"}
	ack, err := f.s.Message(ctx, f.r.Ref, m)
	if err != nil || ack.Stage != Stored {
		t.Fatalf("pre-launch message not stored: %+v %v", ack, err)
	}
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error { j.Phase = Exited; return nil }); err != nil {
		t.Fatal(err)
	}
	ack, err = f.s.Message(ctx, f.r.Ref, m)
	if err != nil || ack.Stage != Failed {
		t.Fatalf("unsent message remained pending after confirmed exit: %+v %v", ack, err)
	}
	if len(f.launcher.starts) != 0 {
		t.Fatal("delivery failure recovery started another process")
	}
}

func TestSSHResolveBlockedReceiptUnblocksQueueWithoutReplay(t *testing.T) {
	for _, stage := range []string{Failed, Uncertain} {
		t.Run(stage, func(t *testing.T) {
			f := testFixture(t)
			f.r.Messaging = true
			ctx := context.Background()
			if _, err := f.s.Start(ctx, f.r); err != nil {
				t.Fatal(err)
			}
			one := Envelope{ID: newID(), Sequence: 1, From: f.r.Ref, To: f.r.Ref, Kind: MessageWake, Text: "blocked"}
			two := one
			two.ID = newID()
			two.Sequence = 2
			if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error {
				if _, err := j.Messages.Store(f.r.Ref, one); err != nil {
					return err
				}
				if _, err := j.Messages.Store(f.r.Ref, two); err != nil {
					return err
				}
				d := j.Messages.Entries[one.ID]
				d.Stage = stage
				j.Messages.Entries[one.ID] = d
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			q := MessageResolution{ID: one.ID, Reason: "operator reconciled uncertain delivery"}
			ack, err := f.s.ResolveMessage(ctx, f.r.Ref, q)
			if err != nil || ack.Stage != Resolved {
				t.Fatalf("resolve failed: %+v %v", ack, err)
			}
			if _, err := f.s.ResolveMessage(ctx, f.r.Ref, q); err != nil {
				t.Fatal(err)
			}
			j, _ := f.s.Read(f.r.Ref)
			if next, ok := j.Messages.Next(); !ok || next.ID != two.ID {
				t.Fatal("blocked cursor did not advance")
			}
			if j.Messages.Entries[one.ID].Stage == Accepted || len(f.launcher.starts) != 0 {
				t.Fatal("resolution claimed consumption or relaunched")
			}
			if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error { return j.Messages.Accept(one.ID) }); err != nil {
				t.Fatal("late echo after resolution killed the stream", err)
			}
			q.Reason = "changed reason"
			if _, err := f.s.ResolveMessage(ctx, f.r.Ref, q); err == nil {
				t.Fatal("conflicting resolution silently replaced audit reason")
			}
		})
	}
}

func TestSSHInspectReportsTurnAndLastActive(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("remote execution hosts require Linux")
	}
	f := testFixture(t)
	f.r.Messaging = true
	gate := filepath.Join(t.TempDir(), "release-wake")
	child := filepath.Join(t.TempDir(), "fake-harness")
	script := "#!/bin/sh\nset -eu\nIFS= read -r first\nprintf '%s\\n' \"$first\" | sed 's/}$/,\"isReplay\":true}/'\nprintf '{\"type\":\"result\"}\\n'\nIFS= read -r second\nprintf '%s\\n' \"$second\" | sed 's/}$/,\"isReplay\":true}/'\nwhile [ ! -e " + quote(gate) + " ]; do sleep 0.01; done\nprintf '{\"type\":\"result\"}\\n'\n"
	if err := os.WriteFile(child, []byte(script), 0o700); err != nil { //nolint:gosec // executable fake harness in its owned temporary directory
		t.Fatal(err)
	}
	h := f.s.Config.Harnesses[HarnessClaude]
	h.Executable = child
	f.s.Config.Harnesses[HarnessClaude] = h
	original := f.s.Command
	f.s.Command = func(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
		if bin == child {
			bin = fixtureClaudeExecutable
		}
		return original(ctx, dir, env, bin, args...)
	}
	digest, err := f.s.Prepare(f.s.Policy())
	if err != nil {
		t.Fatal(err)
	}
	f.r.PolicyDigest = digest
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.s.Run(ctx, f.r.Ref) }()

	// Wait for the first turn's result event to leave the runner idle between turns.
	for {
		o, err := f.s.Inspect(ctx, f.r.Ref)
		if err != nil {
			t.Fatal(err)
		}
		if o.Ready && !o.InTurn && !o.LastActive.IsZero() {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("runner exited before wake: %v", err)
		case <-ctx.Done():
			t.Fatal("first turn did not settle to idle after result")
		case <-time.After(10 * time.Millisecond):
		}
	}

	wake := Envelope{ID: newID(), Sequence: 1, From: f.r.Ref, To: f.r.Ref, Kind: MessageWake, Text: "post-merge follow-up"}
	if _, err := f.s.Message(ctx, f.r.Ref, wake); err != nil {
		t.Fatal(err)
	}
	// While the wake turn is running (before its result event), Inspect reports InTurn.
	for {
		o, err := f.s.Inspect(ctx, f.r.Ref)
		if err != nil {
			t.Fatal(err)
		}
		if o.InTurn && o.MessageSequence == 1 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("runner exited during wake turn: %v", err)
		case <-ctx.Done():
			t.Fatal("wake turn was not observed in-turn")
		case <-time.After(10 * time.Millisecond):
		}
	}

	if err := os.WriteFile(gate, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := ControlRequest{Operation: ControlReport, Data: json.RawMessage(`{"finished":true}`)}
	if err := f.s.finishControl(ctx, f.r.Ref, want, ControlResult{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("runner did not exit after wake turn result and completed report")
	}
	f.launcher.mu.Lock()
	f.launcher.states[UnitName(f.r.Ref)] = unitInactive
	f.launcher.mu.Unlock()
	o, err := f.s.Inspect(ctx, f.r.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if o.InTurn || o.ProcessState != Exited || o.LastActive.IsZero() {
		t.Fatalf("completed runner observation = %+v", o)
	}
}
