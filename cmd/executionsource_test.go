package cmd

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestPrepareExecutionSourcePinsAndRecoversExactCommit(t *testing.T) {
	rig := newPublicationRig(t)
	ctx := context.Background()
	if err := rig.a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec, _ := executionRecord(st, rig.request.Ref)
		rec.Phase = executionPending
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	request, data, err := rig.a.prepareExecutionSource(ctx, rig.request)
	if err != nil {
		t.Fatal(err)
	}
	if request.GitRef != rig.base || request.SourceDigest != execution.Digest(data) {
		t.Fatal("source was not pinned to fetched head")
	}
	st, err := rig.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := executionRecord(st, request.Ref)
	if rec.Request != request || rec.RequestDigest != request.Digest() {
		t.Fatal("pinned request was not durably registered")
	}
	// A later upstream change must not move a recovered execution.
	head := rig.commit(t, "later.txt", "later upstream")
	if _, err := executionPublishGit(ctx, rig.workspace, "push", rig.remote, head+":refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	again, retry, err := rig.a.prepareExecutionSource(ctx, request)
	if err != nil || again != request || execution.Digest(retry) != request.SourceDigest {
		t.Fatalf("recovery changed source: %v", err)
	}
}

func TestExecutionConfirmedMissingPreparationPinsFreshSource(t *testing.T) {
	rig := newPublicationRig(t)
	a := rig.a
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec, _ := executionRecord(st, rig.request.Ref)
		rec.Phase = executionPending
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	host := a.cfg.Agents.Hosts[sshTestSelector]
	handle := execution.Handle{Ref: rig.request.Ref, Repository: rig.request.Repository, Commit: rig.base, Workspace: filepath.Join(host.WorkspaceRoot, string(rig.request.Ref.Execution), "workspace"), Unit: execution.UnitName(rig.request.Ref), ConversationID: rig.request.ConversationID}
	prepared, running := false, false
	uploads, starts := 0, 0
	var digest string
	fake := &testExecutionTransport{}
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		if !prepared {
			return execution.Observation{}, execution.ErrNotFound
		}
		phase := execution.Prepared
		if running {
			phase = execution.Running
		}
		return execution.Observation{Handle: handle, ProcessState: phase, Ready: running}, nil
	}
	fake.start = func(_ context.Context, r execution.StartRequest) (execution.Handle, error) {
		starts++
		if uploads != 1 || r.GitRef != rig.base || r.SourceDigest != digest {
			t.Fatal("fresh preparation preceded pinned source upload")
		}
		prepared = true
		if r.RegistrationReceipt != "" {
			running = true
		}
		return handle, nil
	}
	transport := &sourceRecoveryTransport{testExecutionTransport: fake, upload: func(_ context.Context, ref execution.Ref, data []byte) error {
		uploads++
		digest = execution.Digest(data)
		st, err := a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		rec, _ := executionRecord(st, ref)
		if rec.Request.GitRef != rig.base || rec.Request.SourceDigest != digest || rec.RequestDigest != rec.Request.Digest() {
			t.Fatal("upload preceded durable source pin")
		}
		return nil
	}}
	a.executionTransport = func(execution.Host) execution.Transport { return transport }
	if err := a.activateExecution(context.Background(), rig.request.Ref); err != nil {
		t.Fatal(err)
	}
	if uploads != 1 || starts != 2 || !running {
		t.Fatal("fresh preparation was not activated exactly once")
	}
}
