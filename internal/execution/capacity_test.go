package execution

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

func TestHostAdmissionReconcilesRunnerLoss(t *testing.T) {
	for _, phase := range []string{Activating, Running} {
		for _, state := range []string{unitInactive, Failed, unitActive, "deactivating", ""} {
			t.Run(phase+"/supervisor="+state, func(t *testing.T) {
				f := testFixture(t)
				ctx := context.Background()
				f.s.Config.MaxBusy = 1
				digest, err := f.s.Prepare(f.s.Policy())
				if err != nil {
					t.Fatal(err)
				}
				f.r.PolicyDigest = digest
				f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
				handle, err := f.s.Start(ctx, f.r)
				if err != nil {
					t.Fatal(err)
				}
				// A launch failure or a killed runner cannot commit its terminal
				// journal. Only the owned supervisor has fresh process evidence.
				if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error {
					j.Phase, j.Ready, j.InTurn = phase, phase == Running, true
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				f.launcher.states[handle.Unit] = state
				ended := state == unitInactive || state == Failed
				wantBusy := 1
				if ended {
					wantBusy = 0
				}
				if busy, err := f.s.busy(ctx, Ref{}); err != nil || busy != wantBusy {
					t.Errorf("admission counts %d busy executions, want %d: %v", busy, wantBusy, err)
				}
				busy, release, err := f.s.Quiesce(ctx)
				if err != nil {
					t.Fatal(err)
				}
				release()
				if busy != wantBusy {
					t.Errorf("update counts %d busy executions, want %d", busy, wantBusy)
				}
				next := f.r
				next.Ref.Execution = ExecutionID(uuid.NewString())
				next.RequestID, next.ConversationID, next.RegistrationReceipt = uuid.NewString(), uuid.NewString(), ""
				if _, err := f.s.Start(ctx, next); (err == nil) != ended {
					t.Errorf("successor preparation error = %v, want admission %v", err, ended)
				}
				if _, err := os.Stat(handle.Workspace); err != nil {
					t.Fatalf("reconciliation lost the original workspace: %v", err)
				}
				if len(f.launcher.starts) != 1 || len(f.launcher.stops) != 0 {
					t.Fatal("capacity reconciliation changed an owned process")
				}
			})
		}
	}
}
