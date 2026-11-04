package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSymlinkParentTraversalChecksTheFilesystemTarget(t *testing.T) {
	p, home := policy(t)
	cwd := filepath.Join(home, "projects/repo")
	private := filepath.Join(home, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	const canary = "public audit fixture"
	target := filepath.Join(home, "canary.txt")
	if err := os.WriteFile(target, []byte(canary), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(private, filepath.Join(cwd, "jump")); err != nil {
		t.Fatal(err)
	}
	// Keep the link/.. pair literal, as the broker's raw host read does.
	relative := "jump/../canary.txt"
	absolute := cwd + "/" + relative
	if got, err := filepath.EvalSymlinks(absolute); err != nil || got != target {
		t.Fatalf("actual target = %s, %v; want %s", got, err, target)
	}
	if got, err := os.ReadFile(absolute); err != nil || string(got) != canary { //nolint:gosec // public fixture
		t.Fatalf("raw read = %q, %v", got, err)
	}
	for _, path := range []string{relative, absolute, "jump/../missing/new.txt", cwd + "/jump/../missing/new.txt"} {
		if p.Readable(path, cwd) || p.Writable(path, cwd) {
			t.Errorf("policy opened denied target of %s", path)
		}
	}
	if err := os.Mkdir(filepath.Join(cwd, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"sub/../go.mod", "sub/../new/dir/file.go"} {
		if !p.Readable(path, cwd) || !p.Writable(path, cwd) {
			t.Errorf("ordinary parent traversal refused: %s", path)
		}
	}
}

func TestUnresolvablePathsDoNotAcquireAnAllowance(t *testing.T) {
	p, home := policy(t)
	cwd := filepath.Join(home, "projects/repo")
	if err := os.Symlink("loop", filepath.Join(cwd, "loop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "missing.txt"), filepath.Join(cwd, "dangling")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"loop", "loop/../go.mod", "go.mod/child", "missing/../go.mod", "dangling", ""} {
		if p.Readable(path, cwd) || p.Writable(path, cwd) {
			t.Errorf("unresolvable path allowed: %q", path)
		}
	}
}
