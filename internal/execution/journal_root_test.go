package execution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestExecutionJournalRootIsALiteralPath(t *testing.T) {
	for _, name := range []string{"state[1]", "state*", "state?", "state["} {
		t.Run(name, func(t *testing.T) {
			f := testFixture(t)
			f.s.Root = filepath.Join(t.TempDir(), name, "execution")
			f.s.Config.MaxBusy = 1
			digest, err := f.s.Prepare(f.s.Policy())
			if err != nil {
				t.Fatal(err)
			}
			f.r.PolicyDigest = digest
			ctx := context.Background()
			if _, err := f.s.Start(ctx, f.r); err != nil {
				t.Fatal(err)
			}
			f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
			if _, err := f.s.Start(ctx, f.r); err != nil {
				t.Fatal(err)
			}
			if refs, err := f.s.Inventory(ctx, f.r.Coordinator); err != nil || len(refs) != 1 || refs[0] != f.r.Ref {
				t.Errorf("inventory = %v, %v; want original execution", refs, err)
			}
			busy, release, err := f.s.Quiesce(ctx)
			if err != nil {
				t.Fatal(err)
			}
			release()
			if busy != 1 {
				t.Errorf("quiesce busy = %d, want 1", busy)
			}
			next := f.r
			next.Ref.Execution = ExecutionID(uuid.NewString())
			next.RequestID, next.ConversationID, next.RegistrationReceipt = uuid.NewString(), uuid.NewString(), ""
			if _, err := f.s.Start(ctx, next); err == nil || !strings.Contains(err.Error(), "maxBusy reached") {
				t.Errorf("second preparation = %v; want capacity refusal", err)
			}
			// Non-journal files are ignored, but an unreadable journal must fail closed.
			if err := os.WriteFile(filepath.Join(f.s.Root, "notes.json"), []byte("invalid"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.s.Root, uuid.NewString()+".json"), []byte("invalid"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.Inventory(ctx, f.r.Coordinator); err == nil {
				t.Error("inventory accepted invalid journal")
			}
			if _, err := f.s.busyExecutions(ctx, Ref{}, false); err == nil {
				t.Error("capacity accepted invalid journal")
			}
		})
	}
}
