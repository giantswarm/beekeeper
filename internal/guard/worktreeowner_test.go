package guard

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const worktreeVisitor = "visitor"

func TestWorktreeOwnerCommands(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		cmd, want string
		create    bool
	}{
		{"git commit -am change", dir, false},
		{"git push origin feature", dir, false},
		{"git -C " + dir + " commit -m change", dir, false},
		{"cd " + dir + " && git push origin feature", dir, false},
		{"sh -c 'git commit -am change'", dir, false},
		{"git worktree add -b feature ../new origin/main", filepath.Join(filepath.Dir(dir), "new"), true},
		{"git worktree add --detach -- ../new origin/main", filepath.Join(filepath.Dir(dir), "new"), true},
		{"git worktree add --orphan ../new", filepath.Join(filepath.Dir(dir), "new"), true},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			calls := 0
			h := Hook{mergeSession: worktreeVisitor, WorktreeOwner: func(session, path string, create bool) string {
				calls++
				if session != worktreeVisitor || path != tc.want || create != tc.create {
					t.Fatalf("got %q %q %v", session, path, create)
				}
				return "Refused: owner-session"
			}}
			if r := h.pushRefusal(tc.cmd, dir); !strings.Contains(r, "owner-session") || calls != 1 {
				t.Fatalf("result %q calls %d", r, calls)
			}
		})
	}
	h := Hook{mergeSession: worktreeVisitor, WorktreeOwner: func(string, string, bool) string { t.Fatal("read-only command checked"); return "" }}
	if r := h.pushRefusal("git status", dir); r != "" {
		t.Fatal(r)
	}
}

func TestWorktreeOwnerHook(t *testing.T) {
	h := Hook{WorktreeOwner: func(session, dir string, create bool) string {
		if session != worktreeVisitor {
			t.Fatalf("session %q", session)
		}
		return "Refused: owner-session"
	}}
	raw, err := json.Marshal(map[string]any{sessionKey: worktreeVisitor, "cwd": t.TempDir(), "tool_name": bashTool, executionToolInput: map[string]string{commandKey: "git commit -am change"}})
	if err != nil {
		t.Fatal(err)
	}
	if out := string(h.Decide(raw)); !strings.Contains(out, "owner-session") || !strings.Contains(out, "deny") {
		t.Fatal(out)
	}
}
