package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/watch"
)

// An execution that published more than once keeps one transfer per push. A
// superseded transfer is not an unrecovered publication: only the newest one
// counts, or the host part of retirement never runs.
func TestSupersededPublicationTransferDoesNotHoldRetirement(t *testing.T) {
	rig := newPublicationRig(t)
	ctx := context.Background()
	branch := executionBranch(rig.request.Ref)
	head := rig.commit(t, "pr.txt", "published head\n")
	p := rig.publication(t, head)
	dir := rig.a.publicationTransferDir(rig.request.Ref)
	newest := filepath.Join(dir, strings.TrimPrefix(p.BundleDigest, "sha256:"))
	// An earlier push's transfer, older by time but last by name.
	older := filepath.Join(dir, strings.Repeat("f", 64))
	if err := os.WriteFile(older, []byte("superseded bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(older, past, past); err != nil {
		t.Fatal(err)
	}
	if got := newestPublicationTransfer(dir); got != filepath.Base(newest) {
		t.Fatalf("newest transfer = %q, want %q", got, filepath.Base(newest))
	}

	if err := rig.a.store.Update(func(st *state.State) ([]state.Event, error) {
		record := st.Starts[0].Execution
		record.Published = &state.ExecutionPublication{At: time.Now().UTC(), Branch: branch, Head: head, URL: publicationFixturePRURL}
		record.Observation.ProcessState = execution.Exited
		record.Settled = true
		record.Lease.ExpiresAt = time.Now().Add(-time.Minute)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	// Nothing is stranded: the newest transfer carries the published head.
	// The recovered mark must name that transfer, not the last by name.
	if _, found, err := rig.a.recoverExecutionPublication(ctx, rig.request.Ref, true); err != nil || found {
		t.Fatalf("recovery found=%v err=%v, want nothing stranded", found, err)
	}
	st, err := rig.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	rec := st.Starts[0].Execution
	if rec.RecoveredTransfer != p.BundleDigest {
		t.Fatalf("recovered transfer = %q, want the newest %q", rec.RecoveredTransfer, p.BundleDigest)
	}
	if rig.a.hasUnrecoveredPublication(rec) {
		t.Fatal("a superseded transfer held the execution as unrecovered")
	}

	// A transfer newer than the recovered one still counts.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(older, future, future); err != nil {
		t.Fatal(err)
	}
	if !rig.a.hasUnrecoveredPublication(rec) {
		t.Fatal("a transfer newer than the recovered one was not detected")
	}
}

// Publishing twice in the same turn leaves the second bundle as the newest
// recovered transfer without a separate recovery pass, and the first watch
// sync after completion retires the execution on its host.
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestExecutionRetiresOnHostAfterMultiplePublications(t *testing.T) {
	rig := newPublicationRig(t)
	ctx := context.Background()

	firstHead := rig.commit(t, "pr.txt", "first publication\n")
	firstPub := rig.publication(t, firstHead)
	var firstResult executionPublicationResult
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, firstPub, &firstResult, false); err != nil {
		t.Fatal(err)
	}

	secondHead := rig.commit(t, "pr.txt", "second publication\n")
	secondPub := rig.publication(t, secondHead)
	secondPub.Title, secondPub.Body = "", ""
	var secondResult executionPublicationResult
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, secondPub, &secondResult, false); err != nil {
		t.Fatal(err)
	}

	dir := rig.a.publicationTransferDir(rig.request.Ref)
	if got, want := newestPublicationTransfer(dir), strings.TrimPrefix(secondPub.BundleDigest, "sha256:"); got != want {
		t.Fatalf("newest transfer after second publication = %q, want %q", got, want)
	}

	st, err := rig.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	rec := st.Starts[0].Execution
	if rec.RecoveredTransfer != secondPub.BundleDigest {
		t.Fatalf("recovered transfer = %q, want %q", rec.RecoveredTransfer, secondPub.BundleDigest)
	}
	if rig.a.hasUnrecoveredPublication(rec) {
		t.Fatal("completed second publication left the execution marked unrecovered")
	}

	handle := rec.Handle
	fake := &retireTransport{testExecutionTransport: &testExecutionTransport{
		sync: func(context.Context, execution.Ref) (execution.ControlResult, error) {
			return execution.ControlResult{}, nil
		},
		inspect: func(context.Context, execution.Ref) (execution.Observation, error) {
			return execution.Observation{Handle: handle, ProcessState: execution.Exited, Reachability: execution.Reachable, Ready: true}, nil
		},
		logs: func(context.Context, execution.Ref, execution.LogRequest) (execution.LogPage, error) {
			return execution.LogPage{Data: []byte("turn finished\n"), Next: "14"}, nil
		},
	}}
	rig.a.executionTransport = func(execution.Host) execution.Transport { return fake }

	if err := rig.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Done = true
		st.Starts[0].Execution.Completed = true
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err = rig.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	w := &watch.Watcher{Host: rig.a}
	st = w.SyncExecutions(ctx, st)
	if len(fake.requests) != 1 || fake.requests[0].Trigger != execution.RetireTriggerReport {
		t.Fatalf("host retire requests after syncExecutions = %+v, want 1 report retirement", fake.requests)
	}
	if !st.Starts[0].Execution.Retirement.Finished() {
		t.Fatalf("retirement after syncExecutions = %+v, want finished", st.Starts[0].Execution.Retirement)
	}
}
