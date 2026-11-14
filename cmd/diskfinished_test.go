package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/diskprune"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/gocache"
	"github.com/giantswarm/beekeeper/internal/state"
)

const unknownDiskProof = "unowned"

func TestPressureReclaimsFinishedProofsAndPreservesOpenAndUnknown(t *testing.T) {
	a, m := diskFixture(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{
			{Party: state.Party{Name: "finished disk owner"}, Task: "https://github.com/example/app/issues/123", Done: true},
			{Party: state.Party{Name: "open disk owner"}, Task: "https://github.com/example/app/issues/456"},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"task-123", "task-456", unknownDiskProof} {
		writeT(t, filepath.Join(a.cfg.StateDir, "proofs", name, "output"), strings.Repeat("x", 4096))
	}
	r := diskRun{dry: true}
	a.pruneFinishedState(&r, &m)
	if r.Freed == 0 || !exists(filepath.Join(a.cfg.StateDir, "proofs", "task-123")) {
		t.Fatalf("dry: %+v", r)
	}
	r = diskRun{}
	a.pruneFinishedState(&r, &m)
	if len(r.Errors) != 0 || r.Freed == 0 || exists(filepath.Join(a.cfg.StateDir, "proofs", "task-123")) {
		t.Fatalf("apply: %+v", r)
	}
	for _, name := range []string{"task-456", unknownDiskProof} {
		if !exists(filepath.Join(a.cfg.StateDir, "proofs", name)) {
			t.Fatalf("removed %s", name)
		}
	}
}

func TestHarnessCapKeepsIdleActiveConversation(t *testing.T) {
	a, _ := diskFixture(t)
	a.cfg.Agy.ConversationsDir = t.TempDir()
	a.cfg.Agy.BrainDir = t.TempDir()
	old := a.now.Add(-48 * time.Hour)
	for _, name := range []string{"disk-conversation.pb", "ended.pb"} {
		p := filepath.Join(a.cfg.Agy.ConversationsDir, name)
		writeT(t, p, strings.Repeat("x", 4096))
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	st := &state.State{Starts: []state.Start{{Party: state.Party{Session: "disk-conversation"}, Harness: capacity.HarnessAgy, Conversation: "disk-conversation"}}}
	refs := a.protectHarnessStores(st, nil)
	r := diskprune.Cache(diskprune.Policy{Root: a.cfg.Agy.ConversationsDir, MaxBytes: 1}, a.now, true, refs, nil, false)
	if len(r.Errors) != 0 || !exists(filepath.Join(a.cfg.Agy.ConversationsDir, "disk-conversation.pb")) || exists(filepath.Join(a.cfg.Agy.ConversationsDir, "ended.pb")) {
		t.Fatalf("cap: %+v", r)
	}
}

func TestPressureReclaimsRetiredTransfersStagingAndGoCacheUnderBuilds(t *testing.T) {
	a, m := diskFixture(t)
	retiredRef := execution.Ref{Execution: "exec-retired", Generation: 1}
	activeRef := execution.Ref{Execution: "exec-active", Generation: 1}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = []state.Start{
			{Execution: &state.ExecutionRecord{
				Handle:             execution.Handle{Ref: retiredRef},
				HostJournalMissing: true,
				Retirement:         &state.ExecutionRetirement{RevokedAt: a.now, RetiredAt: a.now},
			}},
			{Execution: &state.ExecutionRecord{
				Handle: execution.Handle{Ref: activeRef},
			}},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	retiredTransfers := filepath.Dir(a.publicationTransferDir(retiredRef))
	activeTransfers := filepath.Dir(a.publicationTransferDir(activeRef))
	pubRoot := filepath.Dir(filepath.Dir(retiredTransfers))
	writeT(t, filepath.Join(retiredTransfers, "1", "bundle"), strings.Repeat("x", 4096))
	writeT(t, filepath.Join(activeTransfers, "1", "bundle"), strings.Repeat("x", 4096))

	staleStage := filepath.Join(pubRoot, "transfer-stale")
	recentStage := filepath.Join(pubRoot, "transfer-recent")
	writeT(t, filepath.Join(staleStage, "pack"), strings.Repeat("x", 4096))
	writeT(t, filepath.Join(recentStage, "pack"), strings.Repeat("x", 4096))
	old := a.now.Add(-2 * time.Hour)
	for _, p := range []string{filepath.Join(staleStage, "pack"), staleStage} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	prevWait := goCacheHoldWait
	goCacheHoldWait = 2 * time.Second
	t.Cleanup(func() { goCacheHoldWait = prevWait })
	releaseUse, err := gocache.Use(context.Background(), m.cache)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		releaseUse()
	}()
	m.builds = true

	r := a.pruneDisk(false, true, func() (diskMachine, error) { return m, nil })
	if len(r.Errors) != 0 {
		t.Fatalf("pressure prune errors: %s", r)
	}
	if exists(retiredTransfers) {
		t.Fatalf("retired publication transfers remained: %s", r)
	}
	if !exists(activeTransfers) {
		t.Fatalf("active publication transfers removed: %s", r)
	}
	if exists(staleStage) || !exists(recentStage) {
		t.Fatalf("staging retention failed (stale=%v recent=%v): %s", exists(staleStage), exists(recentStage), r)
	}
	if exists(filepath.Join(m.cache, "ab", "data")) {
		t.Fatalf("pressure prune did not trim Go cache while builds ran: %s", r)
	}
	if !exists(filepath.Join(m.sccache, "0", "data")) {
		t.Fatalf("sccache trimmed while builds ran: %s", r)
	}
}
