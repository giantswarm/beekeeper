package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The fake coordinator persists receipts independently of the host outbox.
// Dropping a reply after this write models the ambiguous delivery boundary.
type fakeCoordinator struct {
	path  string
	key   string
	drop  bool
	calls int
}
type fakeCoordinatorState struct {
	Sequence uint64                   `json:"sequence"`
	Applied  []string                 `json:"applied"`
	Receipts map[string]ControlResult `json:"receipts"`
}

func (f *fakeCoordinator) run(_ context.Context, argv []string, raw []byte) ([]byte, error) {
	f.calls++
	if !strings.Contains(argv[len(argv)-1], "coordinator-rpc") {
		return nil, errors.New("unexpected callback command")
	}
	var wire CallbackRequest
	if Decode(raw, &wire) != nil || !VerifyCallback(wire, f.key) {
		return nil, errors.New("invalid callback proof")
	}
	var request ControlRequest
	if Decode(wire.Payload, &request) != nil || request.Ref != wire.Ref {
		return nil, errors.New("invalid callback payload")
	}
	var st fakeCoordinatorState
	if err := readJSON(f.path, &st); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	result, ok := st.Receipts[request.ID]
	if ok {
		if result.Digest != request.ContentDigest() || result.Sequence != request.Sequence {
			return nil, errors.New("conflicting receipt")
		}
	} else {
		if request.Sequence != st.Sequence+1 {
			return nil, errors.New("out of order callback")
		}
		st.Sequence = request.Sequence
		st.Applied = append(st.Applied, request.Operation)
		result = ControlResult{ID: request.ID, Sequence: request.Sequence, Digest: request.ContentDigest(), Output: "original receipt"}
		if st.Receipts == nil {
			st.Receipts = map[string]ControlResult{}
		}
		st.Receipts[request.ID] = result
		if err := atomicFile(f.path, mustJSON(st)); err != nil {
			return nil, err
		}
	}
	if f.drop {
		f.drop = false
		return nil, errors.New("reply lost after apply")
	}
	return json.Marshal(WireResponse{Protocol: Protocol, RequestID: wire.RequestID, Ref: wire.Ref, Payload: mustJSON(result)})
}

func TestControlOutboxRecoversLostAcknowledgementInOrder(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &fakeCoordinator{path: filepath.Join(t.TempDir(), "receipts.json"), key: f.r.ControlKey, drop: true}
	data := map[string]string{"question": "fixture question?"}
	if _, err := f.s.SendControl(ctx, j, ControlNote, data, "note-one", coordinator.run); err == nil {
		t.Fatal("lost acknowledgement was reported successful")
	}
	if _, err := f.s.SendControl(ctx, j, ControlPark, map[string]string{"on": ControlNote}, "park-one", coordinator.run); err == nil {
		t.Fatal("later mutation overtook pending note")
	}
	// A fresh Server has no in-memory delivery history.
	restarted := *f.s
	receipt, err := restarted.SyncControl(ctx, f.r.Ref, coordinator.run)
	if err != nil || receipt.Output != "original receipt" || receipt.Sequence != 1 {
		t.Fatal("pending request did not recover its original receipt", err)
	}
	calls := coordinator.calls
	repeated, err := restarted.SendControl(ctx, j, ControlNote, data, "note-one", coordinator.run)
	if err != nil || repeated != receipt || coordinator.calls != calls {
		t.Fatal("acknowledged mutation resent or changed", err)
	}
	if _, err := restarted.SendControl(ctx, j, ControlNote, map[string]string{"question": "different?"}, "note-one", coordinator.run); err == nil {
		t.Fatal("idempotency key accepted conflicting contents")
	}
	if _, err := restarted.SendControl(ctx, j, ControlPark, map[string]string{"on": ControlNote}, "park-one", coordinator.run); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.SendControl(ctx, j, ControlReport, map[string]string{"text": "fixture result"}, "report-one", coordinator.run); err != nil {
		t.Fatal(err)
	}
	var st fakeCoordinatorState
	if err := readJSON(coordinator.path, &st); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(st.Applied, []string{ControlNote, ControlPark, ControlReport}) {
		t.Fatalf("wrong applied order: %v", st.Applied)
	}
	if _, err := restarted.SyncControl(ctx, f.r.Ref, coordinator.run); err != nil {
		t.Fatal(err)
	}
	if coordinator.calls != calls+2 {
		t.Fatal("empty outbox resent an acknowledged request")
	}
}

func TestControlReceiptsSurviveHostOutboxCommitBoundary(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil {
		t.Fatal(err)
	}
	co := &fakeCoordinator{path: filepath.Join(t.TempDir(), "receipts.json"), key: f.r.ControlKey}
	data := json.RawMessage(`{"text":"fixture result"}`)
	result, err := f.s.SendControl(ctx, j, ControlReport, data, "report-one", co.run)
	if err != nil {
		t.Fatal(err)
	}
	// A crash after syncing the receipt but before clearing Pending leaves
	// this old queue. Recovery must clear it without another callback.
	request := ControlRequest{Ref: j.Handle.Ref, ID: result.ID, Sequence: result.Sequence, Operation: ControlReport, Data: data}
	p := filepath.Join(f.s.Root, "control", string(j.Handle.Ref.Execution), "queue.json")
	if err := atomicFile(p, mustJSON(outbox{Next: 1, Pending: &request})); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SyncControl(ctx, f.r.Ref, co.run); err != nil {
		t.Fatal(err)
	}
	if co.calls != 1 {
		t.Fatal("receipt was not authoritative across restart")
	}
}

func TestFailedContactDoesNotInferCompletion(t *testing.T) {
	now := time.Now().UTC()
	before := Observation{ProcessState: Running, Reachability: Reachable, ObservedAt: now.Add(-time.Minute), Ready: true, StopRequested: true}
	after := before.FailedContact(&UnreachableError{Operation: "stop", Cause: errors.New("fixture interruption")}, now)
	if after.ProcessState != Running || after.StopAcknowledged || !after.StopRequested || !after.ObservedAt.Equal(before.ObservedAt) || after.Reachability != Unreachable || !after.ReachabilityAt.Equal(now) {
		t.Fatal("transport loss changed process or stop evidence")
	}
}
