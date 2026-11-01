package changelog

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise Git's actual merge driver with two queued changelog additions.
func TestQueuedChangelogEntriesMerge(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...) //nolint:gosec // fixed executable and literal test arguments
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil { //nolint:gosec // literal filenames inside an owned temporary test repository
			t.Fatal(err)
		}
	}
	git("init", "-b", "main")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	attrs, err := os.ReadFile("../../.gitattributes")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	write(".gitattributes", string(attrs))
	const base = "# Changelog\n\n## [Unreleased]\n\n### Fixed\n\n- Existing entry.\n"
	write("CHANGELOG.md", base)
	write("code.txt", "base\n")
	git("add", ".")
	git("commit", "-m", "base")
	git("checkout", "-b", "first")
	write("CHANGELOG.md", strings.Replace(base, "- Existing", "- First entry.\n- Existing", 1))
	write("code.txt", "first\n")
	git("commit", "-am", "first entry")
	git("checkout", "-b", "second", "main")
	write("CHANGELOG.md", strings.Replace(base, "- Existing", "- Second entry.\n- Existing", 1))
	git("commit", "-am", "second entry")
	git("checkout", "main")
	git("merge", "--no-edit", "first")
	git("merge", "--no-edit", "second")
	merged, err := os.ReadFile(filepath.Join(dir, "CHANGELOG.md")) //nolint:gosec // owned temporary test repository
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"- First entry.", "- Second entry.", "- Existing entry.", "## [Unreleased]", "### Fixed"} {
		if strings.Count(string(merged), entry) != 1 {
			t.Errorf("want exactly one %q in merged changelog:\n%s", entry, merged)
		}
	}
	if got := strings.TrimSpace(git("status", "--porcelain")); got != "" {
		t.Fatalf("unclean merge: %s", got)
	}
	// Other files must retain Git's normal conflict handling.
	git("checkout", "second")
	write("code.txt", "second\n")
	git("commit", "-am", "conflicting code")
	cmd := exec.Command("git", "merge", "main")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "CONFLICT (content): Merge conflict in code.txt") {
		t.Fatalf("expected code conflict, got %v: %s", err, out)
	}
}
