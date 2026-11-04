package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestTrimWorktreesKeepsFilesNamedLikeBuildDirectories(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		name := "legacy"
		if automatic {
			name = "automatic"
		}
		t.Run(name, func(t *testing.T) {
			a, read, root := worktreeApp(t)
			m, err := read()
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, mergedPR)
			path := filepath.Join(dir, "build")
			writeT(t, path, "uncommitted build script")
			m.dirs, m.automatic, m.trash = []string{dir}, automatic, t.TempDir()
			m.retire = func(context.Context, []string, bool) error { return nil }
			r := a.trimWorktrees(false, func() (worktreeMachine, error) { return m, nil })
			if r.err != nil || len(r.failed) > 0 || len(r.removed) != 0 || len(r.cleaned) != 1 {
				t.Fatalf("run=%s failed=%v, want only build output cleaned", r, r.failed)
			}
			if raw, err := os.ReadFile(path); err != nil || string(raw) != "uncommitted build script" { //nolint:gosec // a path of this test
				t.Fatalf("lost untracked script: %q, %v", raw, err)
			}
			if exists(filepath.Join(dir, "node_modules")) {
				t.Fatal("regenerable directory was not cleaned")
			}
		})
	}
}
