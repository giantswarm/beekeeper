package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/diskprune"
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
