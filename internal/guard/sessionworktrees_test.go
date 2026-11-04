package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNestedSessionWorktreeGuard(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(repo, ".claude", "worktrees")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(nested, alias); err != nil {
		t.Fatal(err)
	}
	h := Hook{WorktreePlace: filepath.Join(root, "allowed")}
	for _, cmd := range []string{
		"git worktree add " + nested + "/x",
		"git -C " + repo + " worktree add -b topic .claude/worktrees/x",
		"cd " + repo + " && git worktree add .claude/worktrees/x",
		"W=" + nested + "; git worktree add -B topic \"$W/x\" HEAD",
		"git worktree add " + alias + "/x",
		"bash -c 'git worktree add " + nested + "/x'",
		"for W in " + nested + "/x; do git worktree add \"$W\"; done",
	} {
		d := decide(t, h, repo, cmd, nil)
		if !denied(d) || !strings.Contains(d.Reason, h.WorktreePlace) {
			t.Errorf("%s: %+v", cmd, d)
		}
	}
	for _, cmd := range []string{"git worktree add " + h.WorktreePlace + "/repo/topic", "echo git worktree add " + nested + "/x", "echo test > " + nested + "/note"} {
		if d := decide(t, h, repo, cmd, nil); denied(d) {
			t.Errorf("%s: %+v", cmd, d)
		}
	}
}
