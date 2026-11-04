package claude

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGitInfoOutsideCheckout(t *testing.T) {
	dir := t.TempDir()
	root := dir
	for {
		_, err := os.Stat(filepath.Join(root, ".git"))
		if err == nil {
			t.Skip("temporary directory has a checkout ancestor")
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		parent := filepath.Dir(root)
		if parent == root {
			break
		}
		root = parent
	}
	for name, path := range map[string]string{"directory": dir, "volume root": root} {
		t.Run(name, func(t *testing.T) {
			repo, branch := GitInfo(path)
			if repo != "" || branch != "" {
				t.Fatalf("GitInfo = %q, %q; want empty results", repo, branch)
			}
		})
	}
}

func TestGitInfoCheckout(t *testing.T) {
	dir := t.TempDir()
	gitdir := filepath.Join(dir, ".git")
	if err := os.Mkdir(gitdir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"HEAD":   "ref: refs/heads/topic\n",
		"config": "[remote \"origin\"]\nurl = owner/repo.git\n",
	} {
		if err := os.WriteFile(filepath.Join(gitdir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repo, branch := GitInfo(filepath.Join(dir, "nested", "directory"))
	if repo != "owner/repo" || branch != "topic" {
		t.Fatalf("GitInfo = %q, %q; want owner/repo, topic", repo, branch)
	}
}
