package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPolicyPreparationRejectsSymlinkedParentsBeforeWrites(t *testing.T) {
	f := testFixture(t)
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(f.s.Root), "alias")
	if err := os.Symlink(outside, alias); err != nil {
		t.Skip("symlink unavailable")
	}
	f.s.Root = filepath.Join(alias, "policy")
	if _, err := f.s.Prepare(f.s.Policy()); err == nil {
		t.Fatal("policy preparation followed an aliased parent")
	}
	if _, err := os.Stat(filepath.Join(outside, "policy")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("policy preparation created a directory through the alias")
	}
	if _, err := SigningKey(filepath.Join(alias, "coordinator")); err == nil {
		t.Fatal("signing key creation followed an aliased parent")
	}
	if _, err := os.Stat(filepath.Join(outside, "coordinator")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("key provisioning created a directory through the alias")
	}
}

func TestWorkspaceRejectsSourceAliasInsideExecutionRoot(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	// A mapped source path may look disjoint while resolving into the
	// managed root. Its Git metadata must never become another execution's.
	inside := filepath.Join(f.s.Config.WorkspaceRoot, "source")
	if err := os.Rename(f.source, inside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inside, f.source); err != nil {
		t.Skip("symlink unavailable")
	}
	if _, err := f.s.Start(ctx, f.r); err == nil {
		t.Fatal("source alias inside the execution root was accepted")
	}
	if len(f.launcher.starts) != 0 {
		t.Fatal("process started before source isolation was established")
	}
	if _, err := os.Stat(filepath.Join(f.s.Config.WorkspaceRoot, string(f.r.Ref.Execution))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("workspace created before rejecting the source alias")
	}
}

func TestStopCannotAcknowledgeAnUnreadableSupervisor(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	f.launcher.states[UnitName(f.r.Ref)] = ""
	f.launcher.stopError = true
	request := StopRequest{ID: "fixture-stop", Reason: "fixture stop"}
	if _, err := f.s.Stop(ctx, f.r.Ref, request); err == nil {
		t.Fatal("unreadable termination was acknowledged")
	}
	o, err := f.s.Inspect(ctx, f.r.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if !o.StopRequested || o.StopAcknowledged || o.ProcessState != Unknown || o.Reachability != Reachable {
		t.Fatal("host reachability was confused with owned process termination")
	}
}
