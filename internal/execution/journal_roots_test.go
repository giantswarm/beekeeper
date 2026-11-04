package execution

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"
)

func TestExecutionJournalRootIsALiteralPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX filenames containing star and question mark")
	}
	for _, name := range []string{"state[1]", "state*", "state?"} {
		t.Run(name, func(t *testing.T) {
			f := testFixture(t)
			ctx := context.Background()
			f.s.Root = filepath.Join(filepath.Dir(f.s.Root), name, "execution")
			f.s.Config.MaxBusy = 1
			digest, err := f.s.Prepare(f.s.Policy())
			if err != nil {
				t.Fatal(err)
			}
			f.r.PolicyDigest = digest
			f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
			if _, err := f.s.Start(ctx, f.r); err != nil {
				t.Fatal(err)
			}
			refs, err := f.s.Inventory(ctx, f.r.Coordinator)
			if err != nil || len(refs) != 1 || refs[0] != f.r.Ref {
				t.Errorf("inventory lost its owned execution: %v, %v", refs, err)
			}
			busy, release, err := f.s.Quiesce(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if busy != 1 {
				t.Errorf("update counts %d busy executions, want 1", busy)
			}
			release()
			next := f.r
			next.Ref.Execution = ExecutionID(uuid.NewString())
			next.RequestID, next.ConversationID, next.RegistrationReceipt = uuid.NewString(), uuid.NewString(), ""
			if _, err := f.s.Start(ctx, next); err == nil {
				t.Fatal("second execution bypassed maxBusy under a literal state path")
			}
			// Unrelated state matching the root's pattern must not enter
			// enumeration, even if it contains an unreadable journal.
			neighbor := filepath.Join(filepath.Dir(f.s.Root), "..", "state1", "execution")
			if err := os.MkdirAll(neighbor, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(neighbor, uuid.NewString()+".json"), []byte("unrelated state"), 0o600); err != nil {
				t.Fatal(err)
			}
			if refs, err := f.s.Inventory(ctx, f.r.Coordinator); err != nil || len(refs) != 1 || refs[0] != f.r.Ref {
				t.Errorf("neighbor changed owned inventory: %v, %v", refs, err)
			}
			busy, release, err = f.s.Quiesce(ctx)
			if err != nil {
				t.Fatal(err)
			}
			release()
			if busy != 1 {
				t.Errorf("neighbor changed update capacity to %d, want 1", busy)
			}
			if err := os.WriteFile(filepath.Join(f.s.Root, uuid.NewString()+".json"), []byte("corrupt owned journal"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.Inventory(ctx, f.r.Coordinator); err == nil {
				t.Error("inventory ignored a corrupt journal inside its own root")
			}
			if _, release, err := f.s.Quiesce(ctx); err == nil {
				release()
				t.Error("update admission ignored a corrupt journal inside its own root")
			}
		})
	}
}
