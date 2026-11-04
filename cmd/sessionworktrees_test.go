package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/free"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/worktrees"
)

func nestedFixture(t *testing.T) (*app, string, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{litGitAuthorName, "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "test")
	}
	for _, k := range []string{litGitAuthorEmail, "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "test@example.invalid")
	}
	base := t.TempDir()
	repo := filepath.Join(base, "projects", "org", "repo")
	gitT(t, base, "init", "-q", "-b", "main", repo)
	writeT(t, filepath.Join(repo, ".gitignore"), ".claude/worktrees/\n")
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-qm", "initial")
	cfg := config.Config{StateDir: filepath.Join(base, "state"), Projects: config.Projects{Dir: filepath.Join(base, "projects"), Worktrees: filepath.Join(base, "worktrees")}}
	store, err := state.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	return &app{cfg: &cfg, store: store, out: &bytes.Buffer{}}, repo, base
}

func TestFreeNestedWorktrees(t *testing.T) {
	a, repo, _ := nestedFixture(t)
	root := filepath.Join(repo, ".claude", "worktrees")
	for _, branch := range []string{"merged", "squashed", litDirty, "unpushed"} {
		dir := filepath.Join(root, branch)
		gitT(t, repo, "worktree", "add", "-qb", branch, dir)
		writeT(t, filepath.Join(dir, branch), "first\n")
		gitT(t, dir, "add", ".")
		gitT(t, dir, "commit", "-qm", "change")
		if branch == "merged" {
			gitT(t, repo, "merge", "--no-edit", branch)
		}
		if branch == "squashed" {
			writeT(t, filepath.Join(dir, branch), "second\n")
			gitT(t, dir, "commit", "-qam", "second change")
			gitT(t, repo, "merge", "--squash", branch)
			gitT(t, repo, "commit", "-qm", "squashed")
		}
		if branch == "dirty" {
			writeT(t, filepath.Join(dir, "notes"), "keep me")
		}
	}
	var report bytes.Buffer
	if err := a.freeNestedWorktrees(free.Options{}, &report); err != nil {
		t.Fatal(err)
	}
	if strings.Count(report.String(), "merged and clean, removable") != 2 || strings.Count(report.String(), "unmerged or dirty, to move") != 2 {
		t.Fatal(report.String())
	}
	if err := a.freeNestedWorktrees(free.Options{Apply: true, Only: []string{free.Worktrees}}, &report); err != nil {
		t.Fatal(err)
	}
	if dirs := worktrees.NestedSessions(a.cfg.Projects.Dir); len(dirs) != 0 {
		t.Fatalf("remaining: %v", dirs)
	}
	for _, branch := range []string{"merged", "squashed"} {
		gitT(t, repo, "rev-parse", "--verify", branch) // retained even for squash
		if _, err := os.Stat(filepath.Join(root, branch)); !os.IsNotExist(err) {
			t.Fatal("worktree retained", branch, err)
		}
	}
	moved := filepath.Join(a.cfg.Projects.Worktrees, "repo")
	if b, err := os.ReadFile(filepath.Join(moved, "dirty", "notes")); err != nil || string(b) != "keep me" { //nolint:gosec // a test fixture
		t.Fatalf("dirty work lost: %s %v", b, err)
	}
	if got := gitT(t, filepath.Join(moved, "unpushed"), "log", "-1", "--format=%s"); got != "change" {
		t.Fatal(got)
	}
}

func TestSessionWorktreeHooks(t *testing.T) {
	a, repo, base := nestedFixture(t)
	cfgPath := filepath.Join(base, "config.yaml")
	writeT(t, cfgPath, "stateDir: "+a.cfg.StateDir+"\nprojects:\n  dir: "+a.cfg.Projects.Dir+"\n  worktrees: "+a.cfg.Projects.Worktrees+"\n")
	a.cfgPath = cfgPath
	invoke := func(remove bool, input map[string]string) error {
		raw, _ := json.Marshal(input)
		c := a.sessionWorktreeHook(remove)
		c.SetIn(bytes.NewReader(raw))
		return c.Execute()
	}
	if err := invoke(false, map[string]string{executionOmpCwd: repo, "name": "test"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.cfg.Projects.Worktrees, "repo", "worktree-test")
	if got := strings.TrimSpace(a.out.(*bytes.Buffer).String()); got != path {
		t.Fatalf("stdout %q, want %q", got, path)
	}
	if !worktrees.Linked(path) {
		t.Fatal("not a worktree")
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude", "worktrees")); !os.IsNotExist(err) {
		t.Fatal("nested directory created")
	}
	writeT(t, filepath.Join(path, "notes"), "keep")
	if err := invoke(true, map[string]string{"worktree_path": path}); err == nil {
		t.Fatal("removed dirty tree")
	}
	if err := os.Remove(filepath.Join(path, "notes")); err != nil {
		t.Fatal(err)
	}
	if err := invoke(true, map[string]string{"worktree_path": path}); err != nil {
		t.Fatal(err)
	}
	if err := invoke(false, map[string]string{"cwd": repo, "name": "../../escape"}); err == nil {
		t.Fatal("accepted traversal")
	}
}

func TestSessionRemovableLocked(t *testing.T) {
	_, repo, base := nestedFixture(t)
	dir := filepath.Join(base, "locked")
	gitT(t, repo, "worktree", "add", "-qb", "locked", dir)
	gitT(t, repo, "worktree", "lock", dir)
	ok, err := worktrees.SessionRemovable(context.Background(), runGit, dir)
	if err != nil || ok {
		t.Fatalf("%v %v", ok, err)
	}
}
