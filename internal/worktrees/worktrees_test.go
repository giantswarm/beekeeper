package worktrees

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
)

const dist = "dist/"

func TestClassify(t *testing.T) {
	status := strings.Join([]string{
		" M main.go",
		"R  new.go", "old.go",
		"?? notes.txt",
		"?? web/node_modules/",
		"?? " + dist,
		"!! .turbo/",
		"!! .env",
		"!! .yarn/cache/",
		"!! pkg/cache/",
		"!! pkg/bin/",
	}, "\x00") + "\x00"
	work, build := classify([]byte(status))
	if want := []string{"main.go", "new.go", "notes.txt"}; !slices.Equal(work, want) {
		t.Errorf("work = %q, want %q", work, want)
	}
	if want := []string{"web/node_modules/", dist, ".turbo/", ".yarn/cache/", "pkg/bin/"}; !slices.Equal(build, want) {
		t.Errorf("build = %q, want %q", build, want)
	}
}

func TestHeadBranch(t *testing.T) {
	for _, c := range []struct{ branch, pushed, want string }{
		{"wt/feat/x", "feat/x", "feat/x"},
		{"fix-1", "main", "fix-1"},
		{"fix-2", "", "fix-2"},
	} {
		if got := (Tree{Branch: c.branch, Pushed: c.pushed}).HeadBranch(); got != c.want {
			t.Errorf("HeadBranch(%q tracking %q) = %q, want %q", c.branch, c.pushed, got, c.want)
		}
	}
}

func TestRepoOf(t *testing.T) {
	const repo = "giantswarm/beekeeper"
	for url, want := range map[string]string{
		"https://github.com/giantswarm/beekeeper.git": repo,
		"https://github.com/giantswarm/beekeeper":     repo,
		"git@github.com:giantswarm/beekeeper.git":     repo,
		"ssh://git@github.com/giantswarm/beekeeper":   repo,
		"https://gitlab.com/x/y.git":                  "",
		"/srv/git/repo.git":                           "",
	} {
		if got := RepoOf(url); got != want {
			t.Errorf("RepoOf(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestDecide(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * Grace)
	done := Tree{Repo: "o/r", Branch: "b", Head: "h", Touched: old}
	merged := &github.HeadPull{N: 7, State: github.Merged, HeadOid: "h", ClosedAt: old}
	with := func(f func(*Tree)) Tree { t := done; f(&t); return t }
	for _, c := range []struct {
		name   string
		tree   Tree
		pull   *github.HeadPull
		live   bool
		pushed bool
		want   string
	}{
		{"finished", done, merged, false, true, Remove},
		{"live", done, merged, true, true, Keep},
		{"locked", with(func(t *Tree) { t.Locked = true }), merged, false, true, Keep},
		{"detached and idle", with(func(t *Tree) { t.Branch, t.Touched = "", now.Add(-DetachedIdle) }), nil, false, true, Remove},
		{"detached and recent", with(func(t *Tree) { t.Branch = "" }), nil, false, true, Keep},
		{"detached holding work", with(func(t *Tree) { t.Branch, t.Touched, t.Unpushed = "", now.Add(-DetachedIdle), 6 }), nil, false, true, Keep},
		{"detached holding work and build output", with(func(t *Tree) {
			t.Branch, t.Touched, t.Unpushed, t.Build = "", now.Add(-DetachedIdle), 6, []string{"node_modules/"}
		}), nil, false, true, Clean},
		{"no pull request", done, nil, false, true, Keep},
		{"open", done, &github.HeadPull{N: 7, State: github.Open}, false, true, Keep},
		{"open and idle with build output", with(func(t *Tree) { t.Build = []string{dist} }), &github.HeadPull{N: 7, State: github.Open}, false, true, Clean},
		{"open and recent with build output", with(func(t *Tree) { t.Build, t.Touched = []string{dist}, now }), &github.HeadPull{N: 7, State: github.Open}, false, true, Keep},
		{"live with build output", with(func(t *Tree) { t.Build = []string{dist} }), nil, true, true, Keep},
		{"closed just now", done, &github.HeadPull{N: 7, State: github.Closed, ClosedAt: now}, false, true, Keep},
		{"touched just now", with(func(t *Tree) { t.Touched = now }), merged, false, true, Keep},
		{"work and build output", with(func(t *Tree) { t.Work, t.Build = []string{"a"}, []string{"node_modules/"} }), merged, false, true, Clean},
		{"work only", with(func(t *Tree) { t.Work = []string{"a"} }), merged, false, true, Keep},
		{"unpushed and build output", with(func(t *Tree) { t.Unpushed, t.Build = 1, []string{dist} }), merged, false, false, Clean},
		{"pushed to the pull request", with(func(t *Tree) { t.Unpushed, t.Build = 1, []string{dist} }), merged, false, true, Remove},
	} {
		if got := Decide(c.tree, c.pull, c.live, c.pushed, now); got.Action != c.want {
			t.Errorf("%s: %s (%s), want %s", c.name, got.Action, got.Why, c.want)
		}
	}
}

func TestDangling(t *testing.T) {
	base := t.TempDir()
	gitdir := filepath.Join(base, "repo", ".git", "worktrees", "x")
	if err := os.MkdirAll(gitdir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		gitfile string
		want    bool
	}{
		"present":          {"gitdir: " + gitdir, false},
		"missing":          {"gitdir: " + filepath.Join(base, "gone", ".git", "worktrees", "x"), true},
		"missing relative": {"gitdir: ../gone/.git/worktrees/x", true},
		"no gitdir line":   {"ref: x", false},
	} {
		dir := filepath.Join(base, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte(c.gitfile+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := Dangling(dir); got != c.want {
			t.Errorf("%s: Dangling = %v, want %v", name, got, c.want)
		}
	}
	if Dangling(filepath.Join(base, "repo")) {
		t.Error("a folder without a gitfile is dangling")
	}
}

func TestRemoveAllReadOnlyAndNotThroughLinks(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only directories")
	}
	base := t.TempDir()
	dir, outside := filepath.Join(base, "go"), filepath.Join(base, "outside")
	bin := filepath.Join(dir, "bin", "k8s")
	for _, d := range []string{bin, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "kube-apiserver"), []byte("binary"), 0o500); err != nil { //nolint:gosec // read-only on purpose
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{bin, filepath.Dir(bin), outside} {
		if err := os.Chmod(d, 0o500); err != nil { //nolint:gosec // read-only on purpose
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0o700) }) //nolint:gosec // the test's own directory

	if err := RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("%s is still there: %v", dir, err)
	}
	if fi, err := os.Stat(outside); err != nil || fi.Mode().Perm() != 0o500 {
		t.Errorf("the linked directory outside is %v, %v; want it untouched at 0500", fi.Mode(), err)
	}
}
