package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkingDirectoryAncestorKeepsClosedTreesClosed(t *testing.T) {
	p, home := policy(t)
	p.Runtime = filepath.Join(filepath.Dir(home), "runtime")
	for _, dir := range []string{home, p.Runtime} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "private.txt")
		if err := os.WriteFile(path, []byte("public fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, cwd := range []string{string(filepath.Separator), filepath.Dir(dir)} {
			if p.Readable(path, cwd) || p.Writable(path, cwd) {
				t.Errorf("cwd %s opened %s", cwd, path)
			}
			// Explicit writes and harness memory remain available from an
			// ancestor; an explicit Deny still wins over every allowance.
			for _, allowed := range []string{
				filepath.Join(home, ".local/state/beekeeper/state.json"),
				filepath.Join(home, ".claude/projects/p/memory/note.md"),
			} {
				if !p.Readable(allowed, cwd) || !p.Writable(allowed, cwd) {
					t.Errorf("cwd %s lost explicit allowance for %s", cwd, allowed)
				}
			}
			denied := filepath.Join(home, ".local/state/beekeeper/scan/index.json")
			if p.Readable(denied, cwd) || p.Writable(denied, cwd) {
				t.Errorf("cwd %s opened explicit Deny %s", cwd, denied)
			}
		}
	}
	cwd := filepath.Join(home, "projects/repo")
	if !p.Readable("go.mod", cwd) || !p.Writable("new.go", cwd) {
		t.Error("checkout lost its working-directory allowance")
	}
}
