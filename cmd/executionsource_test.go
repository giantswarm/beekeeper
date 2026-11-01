package cmd

import (
	"context"
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
