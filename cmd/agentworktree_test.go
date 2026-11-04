package cmd

import (
	"context"
	"github.com/giantswarm/beekeeper/internal/config"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartCheckoutRequiresOneMainCheckout(t *testing.T) {
	root := t.TempDir()
	org := filepath.Join(root, "org")
	checkout := filepath.Join(org, "repo")
	if err := os.MkdirAll(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", checkout}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %s: %v", out, err)
		}
	}
	git("init", "-b", "main")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "initial")
	wt := filepath.Join(root, "linked")
	git("worktree", "add", "--detach", wt)
	a := &app{cfg: &config.Config{}}
	for _, dir := range []string{root, org, wt} {
		if _, err := a.startCheckout(dir); err == nil {
			t.Fatalf("accepted %s", dir)
		}
	}
	a.cfg.Agents.Roots = []string{root}
	for _, dir := range []string{root, org, wt} {
		if _, err := a.startCheckout(dir); err == nil || !strings.Contains(err.Error(), checkout) {
			t.Fatalf("refusal must name checkout: %v", err)
		}
	}
	if got, err := a.startCheckout(checkout); err != nil || got != checkout {
		t.Fatalf("checkout = %s, %v", got, err)
	}
}

func TestPrepareStartWorktree(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote")
	checkout := filepath.Join(root, "repo")
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git: %s: %v", out, err)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.Mkdir(remote, 0700); err != nil {
		t.Fatal(err)
	}
	git(remote, "init", "-b", "trunk")
	git(remote, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "initial")
	git(root, "clone", remote, checkout)
	// Advance the remote after cloning: work must start from the fetched tip.
	git(remote, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "new tip")
	tip := git(remote, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(checkout, "personal-edit"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{}}
	a.cfg.StateDir = filepath.Join(root, "state")
	target := filepath.Join(a.cfg.StateDir, "worktrees", "repo", "fix", "task")
	rules, err := a.prepareStartWorktree(ctx, checkout, "fix/task", "task", false, true)
	if err != nil || !strings.Contains(rules, target) {
		t.Fatalf("dry run: %q, %v", rules, err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("dry run created worktree")
	}
	rules, err = a.prepareStartWorktree(ctx, checkout, "fix/task", "task", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rules, "Never commit, stash, or check out") {
		t.Fatal(rules)
	}
	if got := git(target, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("HEAD = %s, want %s", got, tip)
	}
	if got := git(target, "branch", "--show-current"); got != "fix/task" {
		t.Fatal(got)
	}
	if got := git(checkout, "branch", "--show-current"); got != "trunk" {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(checkout, "personal-edit")); err != nil {
		t.Fatal(err)
	}
	git(remote, "update-ref", "refs/pull/42/head", tip)
	_, err = a.prepareStartWorktree(ctx, checkout, "", "Review https://github.com/example/repo/pull/42", true, false)
	if err != nil {
		t.Fatal(err)
	}
	review := filepath.Join(a.cfg.StateDir, "worktrees", "repo", "review-42")
	if got := git(review, "rev-parse", "HEAD"); got != tip {
		t.Fatal(got)
	}
	if got := git(review, "branch", "--show-current"); got != "" {
		t.Fatalf("review not detached: %s", got)
	}
	a.cfg.Agents.WorktreesDir = filepath.Join(root, "custom-worktrees")
	rules, err = a.prepareStartWorktree(ctx, checkout, "custom/task", "task", false, false)
	if err != nil || !strings.Contains(rules, filepath.Join(a.cfg.Agents.WorktreesDir, "repo", "custom/task")) {
		t.Fatalf("custom place: %q, %v", rules, err)
	}
	a.cfg.Agents.WorktreesDir = ""
	if _, err := a.prepareStartWorktree(ctx, checkout, "fix/task", "task", false, false); err == nil {
		t.Fatal("reused existing destination")
	}
}

func TestSingleTaskRepository(t *testing.T) {
	for _, brief := range []string{"task", "https://github.com/example/repo/issues/1 https://github.com/example/repo/pull/2"} {
		if err := singleTaskRepository(brief); err != nil {
			t.Fatal(err)
		}
	}
	for _, brief := range []string{"https://github.com/example/one/issues/1 https://github.com/example/two/issues/2", "example/one#1 and example/two#2"} {
		if err := singleTaskRepository(brief); err == nil || !strings.Contains(err.Error(), "split") {
			t.Fatalf("multi repository task: %v", err)
		}
	}
}
