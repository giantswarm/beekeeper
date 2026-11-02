package execution

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const workspaceOriginalContents = "original\n"

func TestRemoteWorkersOwnLinkedWorktreesAndBranches(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	before, err := f.s.git(ctx, f.source, "show-ref")
	if err != nil {
		t.Fatal(err)
	}
	sourceHead, err := f.s.git(ctx, f.source, "symbolic-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	var branches []string
	var homes []string
	for range 2 {
		r := f.r
		r.Ref.Execution = ExecutionID(uuid.NewString())
		r.RequestID = uuid.NewString()
		r.ConversationID = uuid.NewString()
		h, err := f.transport(t, nil).Start(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		branch, err := f.s.git(ctx, h.Workspace, "symbolic-ref", "--short", "HEAD")
		if err != nil {
			t.Fatalf("worker needs its own branch: %v", err)
		}
		common, err := f.s.git(ctx, h.Workspace, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(h.Workspace, ".git"))
		if err != nil || !info.Mode().IsRegular() {
			t.Fatal("worker is not a linked worktree")
		}
		if !within(filepath.Dir(h.Workspace), strings.TrimSpace(string(common))) {
			t.Fatal("worker shares provisioned Git metadata")
		}
		initial, err := os.ReadFile(filepath.Join(h.Workspace, "tracked.txt"))
		if err != nil || string(initial) != workspaceOriginalContents {
			t.Fatal("worker inherited another workspace change")
		}
		if err := os.WriteFile(filepath.Join(h.Workspace, "tracked.txt"), []byte("worker change"), 0600); err != nil {
			t.Fatal(err)
		}
		branches = append(branches, strings.TrimSpace(string(branch)))
		homes = append(homes, h.Workspace)
	}
	if branches[0] == branches[1] || homes[0] == homes[1] {
		t.Fatal("workers share branch or workspace")
	}
	after, err := f.s.git(ctx, f.source, "show-ref")
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("provisioned refs changed")
	}
	head, err := f.s.git(ctx, f.source, "symbolic-ref", "HEAD")
	if err != nil || !bytes.Equal(sourceHead, head) {
		t.Fatal("provisioned checkout changed branch")
	}
	data, err := os.ReadFile(filepath.Join(f.source, "tracked.txt"))
	if err != nil || string(data) != workspaceOriginalContents {
		t.Fatal("provisioned files changed")
	}
}
