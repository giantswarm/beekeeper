//go:build scenario

package fakegithub

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// origins makes a bare origin with one commit on main for each
// repository, as the scenario harness does, and returns them.
func origins(t *testing.T, repos ...string) *Origins {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH")
	}
	root := t.TempDir()
	home := t.TempDir()
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Scenario", "GIT_AUTHOR_EMAIL=scenario@example.invalid",
		"GIT_COMMITTER_NAME=Scenario", "GIT_COMMITTER_EMAIL=scenario@example.invalid",
	}
	run := func(args ...string) {
		t.Helper()
		c := exec.Command(git, args...) //nolint:gosec // real git on the test's own directories
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	for _, repo := range repos {
		bare := filepath.Join(root, repo+".git")
		seed := t.TempDir()
		run("init", "--bare", "-b", "main", bare)
		run("-C", seed, "init", "-b", "main")
		run("-C", seed, "commit", "--allow-empty", "-m", "initial commit")
		run("-C", seed, "push", bare, "main")
	}
	return &Origins{Git: git, Root: root, Env: env}
}

// A repository's branches, heads, commits, files and merges are its bare
// origin's: a pull request's head is its branch's, a push moves it, a
// merge commits to the base and closes what the pull request closes, and
// compare, contents and the diff read the origin.
func TestHeadsAreTheOrigins(t *testing.T) {
	o := origins(t, "example/app")
	c := &clock{t: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
	s, err := New(parseGiven(t, `
repositories:
  - name: example/app
    issues: [{number: 1, title: One}]
    pulls:
      - {number: 2, title: Add a feature, head: feature, body: "Fixes #1"}
`), Options{Clock: c.Now, Origins: o})
	if err != nil {
		t.Fatal(err)
	}
	head := func() string {
		t.Helper()
		return strings.TrimSpace(mustGH(t, s, "pr", "view", "2", "--repo", "example/app", "--json", "headRefOid", "--jq", ".headRefOid"))
	}
	first := head()
	main := strings.TrimSpace(mustGH(t, s, "api", "repos/example/app/commits/HEAD", "--jq", ".sha"))
	if len(first) != 40 || len(main) != 40 || first == main {
		t.Fatalf("the pull request's head %q is its branch's, a commit beyond main %q", first, main)
	}
	if out := mustGH(t, s, "api", "repos/example/app/git/ref/heads/feature", "--jq", ".object.sha"); strings.TrimSpace(out) != first {
		t.Fatalf("the branch's ref: %q, want %q", out, first)
	}
	view := decode(t, []byte(mustGH(t, s, "pr", "view", "2", "--repo", "example/app", "--json", "files,additions,changedFiles,commits,baseRefOid")))
	if at(t, view, "files", 0, "path") != ".scenario/feature" || view["additions"] != float64(1) || view["changedFiles"] != float64(1) ||
		at(t, view, "commits", 0, "messageHeadline") != "Add a feature" || at(t, view, "commits", 0, "oid") != first || view["baseRefOid"] != main {
		t.Fatalf("the pull request read from its origin: %v", view)
	}

	if err := s.Run(&Step{Push: &PushStep{Repo: "example/app", Branch: "feature", File: "b.txt", Content: "b\n", Message: "Add b"}}); err != nil {
		t.Fatal(err)
	}
	second := head()
	if second == first || len(second) != 40 {
		t.Fatalf("a push moves the head: %q after %q", second, first)
	}
	if out := mustGH(t, s, "api", "repos/example/app/compare/main...feature", "--jq", "[.status, .ahead_by, .behind_by]"); out != `["ahead",2,0]`+"\n" {
		t.Fatalf("compare: %q", out)
	}
	if out := mustGH(t, s, "api", "-H", "Accept: application/vnd.github.raw", "repos/example/app/contents/b.txt?ref=feature"); out != "b\n" {
		t.Fatalf("the file's raw content: %q", out)
	}
	if out := mustGH(t, s, "api", "repos/example/app/contents/b.txt?ref=feature", "--jq", ".content"); strings.TrimSpace(out) != "Ygo=" {
		t.Fatalf("the file's encoded content: %q", out)
	}
	if missing := gh(s, "test", "api", "repos/example/app/contents/b.txt"); missing.Exit != 1 || missing.Stderr != "gh: Not Found (HTTP 404)\n" {
		t.Fatalf("a file main does not have: %+v", missing)
	}
	if diff := mustGH(t, s, "pr", "diff", "2", "--repo", "example/app"); !strings.Contains(diff, "+++ b/b.txt") || !strings.Contains(diff, "+b\n") {
		t.Fatalf("pr diff: %q", diff)
	}
	if names := mustGH(t, s, "pr", "diff", "2", "--repo", "example/app", "--name-only"); names != ".scenario/feature\nb.txt\n" {
		t.Fatalf("pr diff --name-only: %q", names)
	}

	if err := s.Run(&Step{Push: &PushStep{Repo: "example/app", Branch: "main", File: "main.txt", Content: "m\n"}}); err != nil {
		t.Fatal(err)
	}
	if out := mustGH(t, s, "api", "repos/example/app/compare/main...feature", "--jq", ".status"); out != "diverged\n" {
		t.Fatalf("compare after main moved: %q", out)
	}
	if out := mustGH(t, s, "pr", "update-branch", "2", "--repo", "example/app"); out != "✓ PR branch updated\n" {
		t.Fatalf("pr update-branch: %q", out)
	}
	if out := mustGH(t, s, "pr", "update-branch", "2", "--repo", "example/app"); out != "✓ PR branch already up-to-date\n" {
		t.Fatalf("pr update-branch of a head that is up to date: %q", out)
	}
	third := head()
	if out := mustGH(t, s, "api", "repos/example/app/compare/main...feature", "--jq", ".behind_by"); out != "0\n" || third == second {
		t.Fatalf("after the update: behind %q, head %q", out, third)
	}

	if res := gh(s, "test", "pr", "merge", "2", "--repo", "example/app", "--squash", "--delete-branch", "--match-head-commit", third); res.Exit != 0 {
		t.Fatalf("pr merge: %+v", res)
	}
	if err := s.Check(&Expect{Pull: &PullExpect{Repo: "example/app", Number: 2, State: "merged", MergedInto: "main"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(&Expect{Issue: &IssueExpect{Repo: "example/app", Number: 1, State: "closed"}}); err != nil {
		t.Fatal(err)
	}
	merged := decode(t, []byte(mustGH(t, s, "pr", "view", "2", "--repo", "example/app", "--json", "headRefOid,mergeCommit,changedFiles,state")))
	tip := strings.TrimSpace(mustGH(t, s, "api", "repos/example/app/commits/main", "--jq", ".sha"))
	if merged["headRefOid"] != third || at(t, merged, "mergeCommit", "oid") != tip || merged["changedFiles"] != float64(2) {
		t.Fatalf("the merged pull request keeps its head and files; its merge commit is main's: %v, main %s", merged, tip)
	}
	if out := mustGH(t, s, "api", "repos/example/app/commits/main", "--jq", ".commit.message"); !strings.HasPrefix(out, "Add a feature (#2)") {
		t.Fatalf("the squash commit's message: %q", out)
	}
	if out := mustGH(t, s, "api", "-H", "Accept: application/vnd.github.raw", "repos/example/app/contents/b.txt"); out != "b\n" {
		t.Fatalf("main after the merge lacks the branch's file: %q", out)
	}
	if gone := gh(s, "test", "api", "repos/example/app/branches/feature"); gone.Exit != 1 || !strings.Contains(gone.Stderr, "Branch not found") {
		t.Fatalf("the deleted head branch: %+v", gone)
	}
	if un := s.Unsupported(); len(un) != 0 {
		t.Fatalf("unsupported: %+v", un)
	}
}
