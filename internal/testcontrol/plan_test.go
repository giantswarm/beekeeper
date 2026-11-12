//nolint:goconst // Literal paths keep each fixture tree readable as the files it writes.
package testcontrol

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gitRun runs git in dir with a configuration of its own and returns its
// output.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...) //nolint:gosec // git on a test's own repository
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=control", "GIT_AUTHOR_EMAIL=control@example.com",
		"GIT_COMMITTER_NAME=control", "GIT_COMMITTER_EMAIL=control@example.com")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// history commits the tree base and then head on top of it in a new
// repository, and checks base out in a worktree of its own: the pull
// request's checkout and its merge base's.
func history(t *testing.T, base, head map[string]string) (string, string) {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")
	commit := func(tree map[string]string, msg string) {
		writeTree(t, repo, tree)
		gitRun(t, repo, "add", "-A")
		gitRun(t, repo, "commit", "-q", "-m", msg)
	}
	commit(base, "base")
	rev, err := checkedOut(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	commit(head, "head")
	wt := filepath.Join(t.TempDir(), "base")
	gitRun(t, repo, "worktree", "add", "-q", "--detach", wt, rev)
	return repo, wt
}

// Diff lists added, modified and deleted files; a file that becomes a
// symlink is modified.
func TestDiff(t *testing.T) {
	head, base := history(t,
		map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c"},
		map[string]string{"a.txt": "a2", "b.txt": gone, "d/e_test.go": "package d\n"})
	if err := os.Remove(filepath.Join(head, "c.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(head, "c.txt")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, head, "commit", "-q", "-a", "-m", "symlink")
	baseRev, err := checkedOut(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := Diff(t.Context(), head, baseRev, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range changes {
		got = append(got, c.String())
	}
	if want := []string{"M a.txt", "D b.txt", "M c.txt", "A d/e_test.go"}; !slices.Equal(got, want) {
		t.Fatalf("changes %q, want %q", got, want)
	}
	if changes, err := Diff(t.Context(), head, "HEAD", "HEAD"); err != nil || len(changes) != 0 {
		t.Fatalf("no change: %v, %v", changes, err)
	}
}

// The pull request's test files and test data land on the base: copied
// with their mode, a symlink as a symlink, a deleted one removed.
func TestApply(t *testing.T) {
	head, base := t.TempDir(), t.TempDir()
	writeTree(t, base, map[string]string{"x/a_test.go": "old", "x/gone_test.go": "gone"})
	writeTree(t, head, map[string]string{"x/a_test.go": "new", "x/testdata/run.sh": "#!/bin/sh\n"})
	if err := os.Chmod(filepath.Join(head, "x/testdata/run.sh"), 0o500); err != nil { //nolint:gosec // an executable fixture whose mode Apply must keep
		t.Fatal(err)
	}
	if err := os.Symlink("run.sh", filepath.Join(head, "x/testdata/link")); err != nil {
		t.Fatal(err)
	}
	err := Apply([]FileChange{
		{'M', "x/a_test.go"}, {'D', "x/gone_test.go"}, {'D', "x/never_test.go"},
		{'A', "x/testdata/run.sh"}, {'A', "x/testdata/link"},
	}, head, base)
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(base, "x/a_test.go")); err != nil || string(raw) != "new" { //nolint:gosec // the test's own tree
		t.Errorf("x/a_test.go: %q, %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(base, "x/gone_test.go")); !os.IsNotExist(err) {
		t.Errorf("x/gone_test.go is still there: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(base, "x/testdata/run.sh")); err != nil || fi.Mode().Perm() != 0o500 {
		t.Errorf("x/testdata/run.sh: %v, %v", fi, err)
	}
	if target, err := os.Readlink(filepath.Join(base, "x/testdata/link")); err != nil || target != "run.sh" {
		t.Errorf("x/testdata/link: %q, %v", target, err)
	}
}

// The trees of TestPlanControl: a fix of package fixed with its tests, a
// test moved to a new package, tests that build only with tags.
var (
	planBase = map[string]string{
		"go.mod":         "module example.com/plan\n\ngo 1.24\n",
		"fixed/fixed.go": "package fixed\n\nfunc N() int { return 1 }\n",
		"fixed/fixed_test.go": `package fixed

import "testing"

func TestExisting(t *testing.T) { _ = N() }

// TestReworded checks N.
func TestReworded(t *testing.T) {
	if N() == 0 {
		t.Fatal(N())
	}
}

func TestChanged(t *testing.T) { _ = N() }

// control: none: written for the change that added this test
func TestBaseOptOut(t *testing.T) { _ = N() }
`,
		"old/old.go":      "package old\n",
		"old/old_test.go": "package old\n\nimport \"testing\"\n\nfunc TestMoved(t *testing.T) { t.Log(\"moved\") }\n",
		"tags/tags.go":    "package tags\n",
	}
	planHead = map[string]string{
		"fixed/fixed.go": "package fixed\n\nfunc N() int { return 2 }\n",
		"fixed/fixed_test.go": `package fixed

import "testing"

func TestExisting(t *testing.T) { _ = N() }

// TestReworded checks N, which a new comment describes.
func TestReworded(t *testing.T) {
	// N is never zero.

	if N() == 0 {
		t.Fatal(N())
	}
}

func TestChanged(t *testing.T) { _ = N() + 1 }

// control: none: written for the change that added this test
func TestBaseOptOut(t *testing.T) { _ = N() + 1 }

func TestAdded(t *testing.T) { _ = N() }

// TestOptedOut covers what main already does.
//
// control: none: it states behavior main
// already has
func TestOptedOut(t *testing.T) { _ = N() }

// control: none: too short
func TestBadControl(t *testing.T) { _ = N() }
`,
		"fixed/testdata/golden.txt": "2\n",
		"old/old_test.go":           gone,
		"new/new.go":                "package new\n",
		"new/new_test.go":           "package new\n\nimport \"testing\"\n\nfunc TestMoved(t *testing.T) { t.Log(\"moved\") }\n\nfunc TestBrandNew(t *testing.T) {}\n",
		"tags/live_test.go":         "//go:build hyprlive\n\npackage tags\n\nimport \"testing\"\n\nfunc TestLive(t *testing.T) {}\n",
		"tags/tagged_test.go":       "//go:build integration\n\npackage tags\n\nimport \"testing\"\n\nfunc TestTagged(t *testing.T) {}\n",
		"docs/plan.md":              "a page\n",
	}
	planChanges = []FileChange{
		{'M', "docs/plan.md"}, {'M', "fixed/fixed.go"}, {'A', "fixed/testdata/golden.txt"}, {'M', "fixed/fixed_test.go"},
		{'A', "new/new.go"}, {'A', "new/new_test.go"}, {'D', "old/old_test.go"},
		{'A', "tags/live_test.go"}, {'A', "tags/tagged_test.go"},
	}
)

// The plan: which tests the pull request adds, changes or moves, which of
// them the control runs, with which tags, and the files it applies.
func TestPlanControl(t *testing.T) {
	head, base := t.TempDir(), t.TempDir()
	writeTree(t, base, planBase)
	writeTree(t, head, planBase)
	writeTree(t, head, planHead)
	p, err := PlanControl(planChanges, head, base)
	if err != nil {
		t.Fatal(err)
	}
	if !p.CodeChanged {
		t.Error("fixed/fixed.go changed, but the plan sees no code change")
	}
	var overlay []string
	for _, c := range p.Overlay {
		overlay = append(overlay, c.String())
	}
	if want := []string{"A fixed/testdata/golden.txt", "M fixed/fixed_test.go", "A new/new_test.go", "D old/old_test.go", "A tags/live_test.go", "A tags/tagged_test.go"}; !slices.Equal(overlay, want) {
		t.Errorf("overlay %q, want %q", overlay, want)
	}
	type want struct{ change, skip, tags string }
	wants := map[string]want{
		"fixed.TestChanged":    {ChangeChanged, "", ""},
		"fixed.TestBaseOptOut": {ChangeChanged, "", ""},
		"fixed.TestAdded":      {ChangeAdded, "", ""},
		"fixed.TestOptedOut":   {ChangeAdded, SkipOptedOut, ""},
		"fixed.TestBadControl": {ChangeAdded, "", ""},
		"new.TestMoved":        {ChangeMoved, "moved unchanged from old", ""},
		"new.TestBrandNew":     {ChangeAdded, SkipNewPackage, ""},
		"tags.TestLive":        {ChangeAdded, SkipTags, ""},
		"tags.TestTagged":      {ChangeAdded, "", "integration"},
	}
	var got []string
	for _, c := range p.Cases {
		key := c.Dir + "." + c.Name
		got = append(got, key)
		w, ok := wants[key]
		if !ok {
			t.Errorf("unexpected case %+v", c)
			continue
		}
		if c.Change != w.change || c.Skip != w.skip || strings.Join(c.Tags, ",") != w.tags {
			t.Errorf("%s: %q, skip %q, tags %q; want %q, skip %q, tags %q", key, c.Change, c.Skip, c.Tags, w.change, w.skip, w.tags)
		}
	}
	if len(got) != len(wants) {
		t.Errorf("cases %q, want the %d of %v", got, len(wants), wants)
	}
	byKey := map[string]*Case{}
	for _, c := range p.Cases {
		byKey[c.Dir+"."+c.Name] = c
	}
	if c := byKey["fixed.TestOptedOut"]; c == nil || c.Reason != "it states behavior main already has" {
		t.Errorf("TestOptedOut's reason: %+v", c)
	}
	if c := byKey["fixed.TestBaseOptOut"]; c == nil || c.Note != noteBaseOptOut {
		t.Errorf("TestBaseOptOut runs without saying why: %+v", c)
	}
	if len(p.Problems) != 1 || !strings.Contains(p.Problems[0], "TestBadControl") {
		t.Errorf("problems %q, want TestBadControl's control line", p.Problems)
	}
	if run := p.Run(); len(run) != 5 {
		t.Errorf("%d cases to run, want 5", len(run))
	}

	// Without the change to fixed/fixed.go, the base with the pull
	// request's test files is its code: nothing runs.
	var testsOnly []FileChange
	for _, c := range planChanges {
		if c.Path != "fixed/fixed.go" && c.Path != "new/new.go" {
			testsOnly = append(testsOnly, c)
		}
	}
	p, err = PlanControl(testsOnly, head, base)
	if err != nil {
		t.Fatal(err)
	}
	if p.CodeChanged || len(p.Run()) != 0 {
		t.Fatalf("a pull request of tests and docs only: code changed %t, %d cases to run", p.CodeChanged, len(p.Run()))
	}
	for _, c := range p.Cases {
		if c.Change != ChangeMoved && c.Skip != SkipNoCode {
			t.Errorf("%s: skip %q, want %q", c.Name, c.Skip, SkipNoCode)
		}
	}
}
