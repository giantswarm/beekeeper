package execution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRetirementRemovesOnlyDisposableExecutionResources(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	h, err := f.s.Start(ctx, f.r)
	if err != nil {
		t.Fatal(err)
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil {
		t.Fatal(err)
	}
	env, err := f.s.workerEnvironment(j, []string{"TMPDIR=/unowned", "PATH=/bin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range env {
		if (strings.HasPrefix(kv, "TMPDIR=") || strings.HasPrefix(kv, "HOME=")) && !strings.Contains(kv, string(f.r.Ref.Execution)) {
			t.Fatal(kv)
		}
	}
	if err := os.WriteFile(filepath.Join(h.Workspace, ".beekeeper-scratch", "temporary"), []byte("scratch"), 0600); err != nil {
		t.Fatal(err)
	}
	runtime := filepath.Join(filepath.Dir(h.Workspace), "runtime")
	downloaded := filepath.Join(runtime, "home", ".local", "state", "go-toolchain")
	if err := os.MkdirAll(downloaded, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(downloaded, "go"), []byte("download"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtime, "tmp", "scratch"), []byte("temporary"), 0600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(f.s.Config.WorkspaceRoot, uuid.NewString())
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(f.s.Root, string(f.r.Ref.Execution)+".log")
	if err := os.WriteFile(log, []byte("old log"), 0600); err != nil {
		t.Fatal(err)
	}
	old := f.s.now().Add(-executionLogRetention - time.Hour)
	if err := os.Chtimes(log, old, old); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := f.s.Stop(ctx, f.r.Ref, StopRequest{ID: "retire", Reason: "finished"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{filepath.Dir(h.Workspace), log} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("disposable path remains: %s: %v", p, err)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("unrelated execution removed", err)
	}
	j, err = f.s.Read(f.r.Ref)
	if err != nil || j.Cleanup == nil || j.Cleanup.Error != "" || j.Cleanup.Retained {
		t.Fatalf("cleanup: %+v, %v", j.Cleanup, err)
	}
	findings, err := f.s.AuditLeftovers()
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Class != "unknown" {
		t.Fatalf("audit: %+v", findings)
	}
}

func TestRetirementPreservesIgnoredFilesAndReflogCommits(t *testing.T) {
	for _, mode := range []string{"ignored", "reflog"} {
		t.Run(mode, func(t *testing.T) {
			f := testFixture(t)
			ctx := context.Background()
			h, err := f.s.Start(ctx, f.r)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "ignored" {
				if err := os.WriteFile(filepath.Join(filepath.Dir(h.Workspace), "repository.git", "info", "exclude"), []byte("precious\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(h.Workspace, "precious"), []byte("work"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				for _, args := range [][]string{{"-c", "user.name=Example", "-c", "user.email=example@example.invalid", "commit", "--allow-empty", "-m", "unpublished"}, {"reset", "--hard", h.Commit}} {
					if _, err := f.s.git(ctx, h.Workspace, args...); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := f.s.Stop(ctx, f.r.Ref, StopRequest{ID: "retain", Reason: "stopped"}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(h.Workspace); err != nil {
				t.Fatal("recovery lost", err)
			}
			j, err := f.s.Read(f.r.Ref)
			if err != nil || j.Cleanup == nil || !j.Cleanup.Retained {
				t.Fatalf("recovery not recorded: %+v %v", j.Cleanup, err)
			}
			findings, err := f.s.AuditLeftovers()
			if err != nil || len(findings) != 1 || findings[0].Class != "recovery" {
				t.Fatalf("audit %+v %v", findings, err)
			}
		})
	}
}

func TestRetirementRefusesSymlinkAndRetries(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	h, err := f.s.Start(ctx, f.r)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	runtime := filepath.Join(filepath.Dir(h.Workspace), "runtime")
	if err := os.Symlink(outside, runtime); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Stop(ctx, f.r.Ref, StopRequest{ID: "stop", Reason: "finished"}); err != nil {
		t.Fatal(err)
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil || j.Cleanup == nil || j.Cleanup.Error == "" {
		t.Fatalf("missing retry state: %+v %v", j.Cleanup, err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("followed symlink", err)
	}
	if err := os.Remove(runtime); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Stop(ctx, f.r.Ref, StopRequest{ID: "stop", Reason: "finished"}); err != nil {
		t.Fatal(err)
	}
	j, err = f.s.Read(f.r.Ref)
	if err != nil || j.Cleanup.Error != "" {
		t.Fatalf("retry failed: %+v %v", j.Cleanup, err)
	}
}

func TestRetirementReconciliationPreservesFailureAndOtherWorker(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	first, err := f.s.Start(ctx, f.r)
	if err != nil {
		t.Fatal(err)
	}
	secondRequest := f.r
	secondRequest.Ref.Execution = ExecutionID(uuid.NewString())
	secondRequest.RequestID = uuid.NewString()
	// Preparation signatures bind the full request.
	secondRequest.RegistrationReceipt = ""
	second, err := f.s.Start(ctx, secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error {
		j.Phase = Failed
		j.ActivationAttempted = true
		j.LastError = "harness failed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.launcher.states[first.Unit] = unitInactive
	if _, err := f.s.Inspect(ctx, f.r.Ref); err != nil {
		t.Fatal(err)
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil || j.Phase != Failed || j.LastError != "harness failed" || j.Cleanup == nil {
		t.Fatalf("failure evidence changed: %+v %v", j, err)
	}
	if _, err := os.Stat(second.Workspace); err != nil {
		t.Fatal("other worker removed", err)
	}
	if _, err := os.Stat(f.s.Config.Repositories[f.r.Repository]); err != nil {
		t.Fatal("provisioned source removed", err)
	}
}

func TestRetirementAuditIsReadOnlyForSymlinksAndParkedWorkers(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	h, err := f.s.Start(ctx, f.r)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error { j.Phase = Running; j.Ready = true; j.InTurn = false; return nil }); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	link := filepath.Join(f.s.Config.WorkspaceRoot, "unclassified")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	findings, err := f.s.AuditLeftovers()
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string]bool{}
	for _, f := range findings {
		classes[f.Class] = true
	}
	if !classes["parked"] || !classes["unknown"] {
		t.Fatalf("audit: %+v", findings)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("audit changed symlink", err)
	}
	if _, err := os.Stat(h.Workspace); err != nil {
		t.Fatal("audit removed parked workspace", err)
	}
}
