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
