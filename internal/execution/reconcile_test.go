package execution

import (
	"bytes"
	"context"
	"errors"
	"github.com/google/uuid"
	"os"
	"testing"
)

func TestInventorySeparatesCoordinatorsAndStoppedJournals(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	refs, err := f.s.Inventory(f.r.Coordinator)
	if err != nil || len(refs) != 1 || refs[0] != f.r.Ref {
		t.Fatalf("inventory=%v %v", refs, err)
	}
	f.s.Config.Coordinators["other-coordinator"] = f.s.Config.Coordinators[f.r.Coordinator]
	if refs, err := f.s.Inventory("other-coordinator"); err != nil || len(refs) != 0 {
		t.Fatal("inventory included another coordinator's journal")
	}
	if _, err := f.s.Inventory("untrusted"); err == nil {
		t.Fatal("accepted unknown coordinator")
	}
	if _, err := f.s.Stop(ctx, f.r.Ref, StopRequest{ID: "fixture-orphan", Reason: "orphan"}); err != nil {
		t.Fatal(err)
	}
	refs, err = f.s.Inventory(f.r.Coordinator)
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
}
