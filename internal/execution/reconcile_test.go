package execution

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
)

func TestInventorySeparatesCoordinatorsAndStoppedJournals(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	refs, err := f.s.Inventory(ctx, f.r.Coordinator)
	if err != nil || len(refs) != 1 || refs[0] != f.r.Ref {
		t.Fatalf("inventory=%v %v", refs, err)
	}
	f.s.Config.Coordinators["other-coordinator"] = f.s.Config.Coordinators[f.r.Coordinator]
	if refs, err := f.s.Inventory(ctx, "other-coordinator"); err != nil || len(refs) != 0 {
		t.Fatal("inventory included another coordinator's journal")
	}
	if _, err := f.s.Inventory(ctx, "untrusted"); err == nil {
		t.Fatal("accepted unknown coordinator")
	}
	// A naturally exited execution with an inactive unit is not an active
	// orphan and must not be returned for synthetic stop mutation.
	f.launcher.states[UnitName(f.r.Ref)] = unitInactive
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error {
		j.Phase, j.Ready = Exited, true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if refs, err := f.s.Inventory(ctx, f.r.Coordinator); err != nil || len(refs) != 0 {
		t.Fatalf("naturally exited journal returned by inventory: %v %v", refs, err)
	}
	f.launcher.states[UnitName(f.r.Ref)] = unitActive
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error {
		j.Phase = Running
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Stop(ctx, f.r.Ref, StopRequest{ID: "fixture-orphan", Reason: "orphan"}); err != nil {
		t.Fatal(err)
	}
	refs, err = f.s.Inventory(ctx, f.r.Coordinator)
	if err != nil || len(refs) != 0 {
		t.Fatalf("stopped journal remains: %v %v", refs, err)
	}
}

func TestMissingJournalRequiresInactiveUnit(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	if absent, err := f.s.Missing(ctx, f.r.Ref); err != nil || absent {
		t.Fatalf("existing=%v %v", absent, err)
	}
	p, err := f.s.journalPath(f.r.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p, p+".aside"); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{unitActive, "", unitInactive, Failed} {
		f.launcher.states[UnitName(f.r.Ref)] = status
		absent, err := f.s.Missing(ctx, f.r.Ref)
		want := status == unitInactive || status == Failed
		if err != nil || absent != want {
			t.Fatalf("%q missing=%v %v", status, absent, err)
		}
	}
	if len(f.launcher.stops) != 0 {
		t.Fatal("stopped unit without ownership journal")
	}
}

func TestInventoryOverSSHWire(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	transport := SSHTransport{Host: f.host, Run: func(ctx context.Context, _ []string, input []byte) ([]byte, error) {
		var out bytes.Buffer
		err := f.s.Serve(ctx, bytes.NewReader(input), &out)
		return out.Bytes(), err
	}}
	refs, err := transport.Inventory(ctx, f.r.Coordinator)
	if err != nil || len(refs) != 1 || refs[0] != f.r.Ref {
		t.Fatalf("wire inventory=%v %v", refs, err)
	}
	if absent, err := transport.Missing(ctx, f.r.Ref); err != nil || absent {
		t.Fatalf("wire missing=%v %v", absent, err)
	}
	missing := f.r.Ref
	missing.Execution = ExecutionID(uuid.NewString())
	if _, err := transport.SyncControl(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("sync missing error=%v", err)
	}
	if _, err := transport.Stop(ctx, missing, StopRequest{ID: "stop-missing", Reason: "missing stop"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stop missing error=%v", err)
	}
	// Pre-inventory hosts return os.ErrNotExist text without reply.Code on sync-control and stop.
	legacy := SSHTransport{Host: f.host, Run: func(_ context.Context, _ []string, input []byte) ([]byte, error) {
		var req WireRequest
		if err := Decode(input, &req); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err := f.s.Serve(ctx, bytes.NewReader(input), &out)
		if err != nil {
			return nil, err
		}
		var reply WireResponse
		if err := Decode(out.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		if req.Operation != inspectOperation {
			reply.Code = ""
		}
		return mustJSON(reply), nil
	}}
	if _, err := legacy.SyncControl(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy sync missing error=%v", err)
	}
	if _, err := legacy.Stop(ctx, missing, StopRequest{ID: "stop-missing", Reason: "legacy stop"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy stop missing error=%v", err)
	}
}
