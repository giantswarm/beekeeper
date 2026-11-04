package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestRetirementLifecycle(t *testing.T) {
	for _, mode := range []string{"stop", "failed start", "crash", "interrupted rename", "parked"} {
		t.Run(mode, func(t *testing.T) {
			f := testFixture(t)
			ctx := context.Background()
			h, err := f.s.Start(ctx, f.r)
			if err != nil {
				t.Fatal(err)
			}
			env, err := f.s.workerEnvironment(f.r.Ref, nil)
			if err != nil || len(env) != 7 {
				t.Fatalf("owned environment: %v %v", env, err)
			}
			runtime := filepath.Join(f.s.ownedRoot(f.r.Ref), "runtime")
			dirty := filepath.Join(h.Workspace, "untracked-recovery.txt")
			if err := os.WriteFile(dirty, []byte("recover me"), 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "interrupted rename" {
				if err := os.Rename(runtime, runtime+".retired"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "stop" {
				if _, err := f.s.Stop(ctx, f.r.Ref, StopRequest{ID: uuid.NewString(), Reason: "test"}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error {
					if mode == "failed start" {
						j.Phase = Failed
					} else {
						j.Phase, j.ActivationAttempted, j.Ready = Running, true, true
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				f.launcher.states[h.Unit] = unitInactive
				if mode == "parked" {
					f.launcher.states[h.Unit] = unitActive
				}
			}
			for range 2 {
				if err := f.s.Retire(ctx, f.r.Ref); err != nil {
					t.Fatal(err)
				}
			}
			inv, err := f.s.resourceInventory(f.r.Ref)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "parked" {
				if !inv.Started.IsZero() || inv.Recovery != "" {
					t.Fatal("parked worker retired")
				}
				if _, err := os.Stat(runtime); err != nil {
					t.Fatal(err)
				}
				return
			}
			if inv.Finished.IsZero() || !inv.Revoked {
				t.Fatalf("unfinished retirement: %+v", inv)
			}
			if _, err := os.Stat(inv.Recovery); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(dirty); err != nil || string(got) != "recover me" {
				t.Fatalf("lost work: %q %v", got, err)
			}
			if _, err := os.Stat(runtime + ".retired"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRetirementRejectsSymlinkAndForeignInventory(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.workerEnvironment(f.r.Ref, nil); err != nil {
		t.Fatal(err)
	}
	inv, err := f.s.resourceInventory(f.r.Ref)
	if err != nil {
		t.Fatal(err)
	}
	foreign := f.r.Ref
	foreign.Generation++
	if _, err := f.s.resourceInventory(foreign); err == nil {
		t.Fatal("adopted another generation")
	}
	inv.Resources[0].Path = f.source
	if err := atomicFile(f.s.inventoryPath(f.r.Ref), mustJSON(inv)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.resourceInventory(f.r.Ref); err == nil {
		t.Fatal("accepted provisioned source")
	}
	inv.Resources = f.s.resourcePaths(f.r.Ref)
	if err := atomicFile(f.s.inventoryPath(f.r.Ref), mustJSON(inv)); err != nil {
		t.Fatal(err)
	}
	// A caller-controlled resource must never become authority to rename its target.
	handleDir := filepath.Join(f.s.Root, workerGitHubBinDir, string(f.r.Ref.Execution))
	if err := os.MkdirAll(filepath.Dir(handleDir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.source, handleDir); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Stop(ctx, f.r.Ref, StopRequest{ID: uuid.NewString(), Reason: "test"}); err == nil {
		t.Fatal("retired symlink")
	}
	if _, err := os.Stat(filepath.Join(f.source, "tracked.txt")); err != nil {
		t.Fatal(err)
	}
	inv, err = f.s.resourceInventory(f.r.Ref)
	if err != nil || inv.LastError == "" || !inv.Finished.IsZero() {
		t.Fatalf("failure not resumable: %+v %v", inv, err)
	}
}

func TestRetirementRevocationRetry(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	f.r.GitHub = true
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	f.s.Callback = func(context.Context, []string, []byte) ([]byte, error) {
		return nil, errors.New("coordinator unavailable")
	}
	stop := StopRequest{ID: uuid.NewString(), Reason: "test"}
	if _, err := f.s.Stop(ctx, f.r.Ref, stop); err == nil {
		t.Fatal("reported revocation success while unavailable")
	}
	inv, err := f.s.resourceInventory(f.r.Ref)
	if err != nil || inv.Revoked || !inv.Finished.IsZero() || inv.LastError == "" {
		t.Fatalf("lost pending revocation: %+v %v", inv, err)
	}
	f.s.Callback = func(_ context.Context, _ []string, data []byte) ([]byte, error) {
		var req CallbackRequest
		if err := Decode(data, &req); err != nil {
			return nil, err
		}
		return json.Marshal(WireResponse{Protocol: Protocol, RequestID: req.RequestID, Ref: req.Ref, Payload: json.RawMessage(`{}`)})
	}
	if _, err := f.s.Stop(ctx, f.r.Ref, stop); err != nil {
		t.Fatal(err)
	}
	inv, err = f.s.resourceInventory(f.r.Ref)
	if err != nil || !inv.Revoked || inv.Finished.IsZero() || inv.LastError != "" {
		t.Fatalf("retry failed: %+v %v", inv, err)
	}
}

func TestRetirementLeavesOtherWorkerAndProvisionedRepository(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	other := f.r
	other.Ref.Execution = ExecutionID(uuid.NewString())
	other.ConversationID = uuid.NewString()
	other.RequestID = uuid.NewString()
	h, err := f.s.Start(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.workerEnvironment(other.Ref, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Stop(ctx, f.r.Ref, StopRequest{ID: uuid.NewString(), Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(h.Workspace, "tracked.txt"), filepath.Join(f.source, "tracked.txt"), filepath.Join(f.s.ownedRoot(other.Ref), "runtime")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	inv, err := f.s.resourceInventory(other.Ref)
	if err != nil || !inv.Started.IsZero() {
		t.Fatalf("other worker retired: %+v %v", inv, err)
	}
}

func TestFailedPreparationCanRetireWithoutDeletingPartialClone(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	f.r.GitRef = "refs/heads/does-not-exist"
	if _, err := f.s.Start(ctx, f.r); err == nil {
		t.Fatal("preparation unexpectedly succeeded")
	}
	repository := filepath.Join(f.s.Config.WorkspaceRoot, string(f.r.Ref.Execution), "repository.git")
	if _, err := os.Stat(repository); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Inspect(ctx, f.r.Ref); err != nil {
		t.Fatal(err)
	}
	inv, err := f.s.resourceInventory(f.r.Ref)
	if err != nil || inv.Finished.IsZero() {
		t.Fatalf("failed preparation not retired: %+v %v", inv, err)
	}
	if _, err := os.Stat(repository); err != nil {
		t.Fatal("partial clone lost", err)
	}
}

func TestResourceInventoryDoesNotAdoptOperatorFiles(t *testing.T) {
	f := testFixture(t)
	path := filepath.Join(f.s.ownedRoot(f.r.Ref), "runtime")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Start(context.Background(), f.r); err == nil {
		t.Fatal("adopted unrecorded files")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestRetirementRetryRejectsSubstitutedDestination(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	runtime := filepath.Join(f.s.ownedRoot(f.r.Ref), "runtime")
	if err := os.MkdirAll(filepath.Dir(runtime), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.source, runtime+".retired"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Stop(ctx, f.r.Ref, StopRequest{ID: uuid.NewString(), Reason: "test"}); err == nil {
		t.Fatal("accepted substituted retirement destination")
	}
	inv, err := f.s.resourceInventory(f.r.Ref)
	if err != nil || inv.LastError == "" || !inv.Finished.IsZero() {
		t.Fatalf("failure not resumable: %+v %v", inv, err)
	}
	if _, err := os.Stat(filepath.Join(f.source, "tracked.txt")); err != nil {
		t.Fatal(err)
	}
}
