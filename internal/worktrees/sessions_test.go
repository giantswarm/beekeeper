package worktrees

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionPath(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(root, "external")
	p, err := SessionPath(external, repo, "topic/change")
	if err != nil || p != filepath.Join(external, "repo", "topic", "change") {
		t.Fatalf("%s %v", p, err)
	}
	for _, b := range []string{"", "../escape", "topic/../escape", "/absolute", "-option"} {
		if _, err := SessionPath(external, repo, b); err == nil {
			t.Errorf("accepted branch %q", b)
		}
	}
	nested := filepath.Join(repo, ".claude", "worktrees")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(nested, alias); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{nested, alias} {
		if _, err := SessionPath(target, repo, "topic"); err == nil {
			t.Errorf("accepted nested root %s", target)
		}
	}
}
