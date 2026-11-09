package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/fatal"
)

const testBranchFlag = "--branch"

var testStartBranches atomic.Int64

// testGitPath is git as found before a test narrows PATH to its stubs.
var testGitPath, _ = exec.LookPath("git")

// keepTestGit adds git alone to a PATH of stubs that lacks it.
func keepTestGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err == nil {
		return
	}
	dir := t.TempDir()
	if err := os.Symlink(testGitPath, filepath.Join(dir, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", os.Getenv("PATH")+string(os.PathListSeparator)+dir)
}

// testCheckoutArgs makes dir a repository checkout with an origin and adds a
// branch of its own to the arguments of a hand start there.
func testCheckoutArgs(t *testing.T, dir string, args []string) []string {
	t.Helper()
	keepTestGit(t)
	if !checkoutDir(dir) {
		origin := t.TempDir()
		git := func(in string, args ...string) {
			t.Helper()
			if _, err := runGit(context.Background(), in, append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.invalid"}, args...)...); err != nil {
				t.Fatal(err)
			}
		}
		git(origin, "init", "--bare", "-b", "main")
		git(dir, "init", "-b", "main")
		git(dir, "commit", "--allow-empty", "-m", "initial")
		git(dir, "remote", "add", "origin", origin)
		git(dir, "push", "origin", "main", "main:refs/pull/1/head")
	}
	return append(args, testBranchFlag, fmt.Sprintf("test-start-%d", testStartBranches.Add(1)))
}

func TestStartCheckoutRequiresOneMainCheckout(t *testing.T) {
	root := t.TempDir()
	org := filepath.Join(root, "org")
	checkout := filepath.Join(org, "repo")
	if err := os.MkdirAll(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		if _, err := runGit(context.Background(), checkout, args...); err != nil {
			t.Fatal(err)
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

// The supervisor loop knows a repository, not a checkout: its starts must pass
// where the same start by hand is refused, and name no worktree.
func TestLoopStartKeepsAgentsDir(t *testing.T) {
	r := newPickRig(t, `    - {harness: claude, model: opus-x}
`)
	parent := filepath.Join(r.dir, "projects")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(r.dir, "brief.md")
	if err := os.WriteFile(brief, []byte("https://github.com/example/one/issues/1 https://github.com/example/two/issues/2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := func(as string) (string, error) {
		a, out := r.app(false)
		a.cfg.Agents.Dir = parent
		a.as = as
		c := a.agentStartCmd()
		c.SilenceErrors, c.SilenceUsage = true, true
		c.SetArgs([]string{"Picked", brief, testDryRunFlag})
		err := c.Execute()
		return out.String(), err
	}
	if _, err := start(""); err == nil || !strings.Contains(err.Error(), "exactly one repository checkout") {
		t.Fatalf("hand start: %v", err)
	}
	out, err := start(loopParty.Name)
	if err != nil {
		t.Fatalf("loop start: %v", err)
	}
	if strings.Contains(out, "Work only in the worktree") {
		t.Fatalf("loop start names a worktree:\n%s", out)
	}
}

func TestPrepareStartWorktree(t *testing.T) {
	const preservedContents = "keep"
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote")
	checkout := filepath.Join(root, "repo")
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := runGit(ctx, dir, args...)
		if err != nil {
			t.Fatal(err)
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
	if err := os.WriteFile(filepath.Join(checkout, "personal-edit"), []byte(preservedContents), 0600); err != nil {
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
	// A stale legacy review checkout, even with edits, must not block a start.
	legacy := filepath.Join(a.cfg.StateDir, "worktrees", "repo", "review-42")
	git(checkout, "worktree", "add", "--detach", legacy, tip)
	if err := os.WriteFile(filepath.Join(legacy, "review-edits"), []byte(preservedContents), 0600); err != nil {
		t.Fatal(err)
	}
	rules, err = a.prepareStartWorktree(ctx, checkout, "", "Review https://github.com/example/repo/pull/42", true, false)
	if err != nil {
		t.Fatal(err)
	}
	reviews, err := filepath.Glob(legacy + "-*")
	if err != nil || len(reviews) != 1 {
		t.Fatalf("review destinations: %v, %v", reviews, err)
	}
	review := reviews[0]
	if !strings.Contains(rules, review) {
		t.Fatalf("brief lacks checkout: %s", rules)
	}
	if got := git(review, "rev-parse", "HEAD"); got != tip {
		t.Fatal(got)
	}
	if got := git(review, "branch", "--show-current"); got != "" {
		t.Fatalf("review not detached: %s", got)
	}
	// A second attempt must also leave the previous attempt intact.
	nextRules, err := a.prepareStartWorktree(ctx, checkout, "", "Review https://github.com/example/repo/pull/42", true, false)
	if err != nil || nextRules == rules {
		t.Fatalf("repeat review: %q, %v", nextRules, err)
	}
	if got := git(review, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("prior checkout changed: %s", got)
	}
	if data, err := os.ReadFile(filepath.Join(legacy, "review-edits")); err != nil || string(data) != preservedContents { //nolint:gosec // fixture path constructed entirely under t.TempDir
		t.Fatalf("legacy edits changed: %q, %v", data, err)
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
	for _, brief := range []string{paramTask, "https://github.com/example/repo/issues/1 https://github.com/example/repo/pull/2"} {
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

// A late admission refusal must not reserve the requested branch or checkout.
func TestStartFatalRefusalLeavesWorktreeAvailable(t *testing.T) {
	r := newPickRig(t, "    - {harness: claude, model: opus-x}\n")
	bin := t.TempDir()
	if err := os.Symlink("/bin/true", filepath.Join(bin, claudeComm)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	brief := filepath.Join(r.dir, "brief.md")
	if err := os.WriteFile(brief, []byte("Implement a worker fix\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args := testCheckoutArgs(t, r.dir, []string{"refused-worker", brief, usageDirFlag, r.dir, testHarnessFlag, claudeComm, usageForceFlag})
	branch := args[len(args)-1]
	a, _ := r.app(false)
	q := fatal.Queue{}
	q.Observe(fatal.Observation{ID: "installed-guard-check", Failed: true}, a.now)
	if err := q.Save(a.store.Dir()); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(r.dir, "worktrees", filepath.Base(r.dir), branch)
	a.cfg.Agents.WorktreesDir = filepath.Join(r.dir, "worktrees")
	for range 2 {
		c := a.agentStartCmd()
		c.SilenceErrors, c.SilenceUsage = true, true
		c.SetArgs(args)
		if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "takes priority") {
			t.Fatalf("start refusal: %v", err)
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Fatalf("refused start left worktree: %v", err)
		}
		out, err := runGit(t.Context(), r.dir, "branch", "--list", branch)
		if err != nil || strings.TrimSpace(string(out)) != "" {
			t.Fatalf("refused start left branch: %s, %v", out, err)
		}
	}
}
