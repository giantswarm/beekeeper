package free

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

func TestFreeKeepsHeldDirsThroughTempAlias(t *testing.T) {
	// Exercise an aliased test-runner root as well as the configured root.
	// This also occurs on hosts where /var resolves to /private/var.
	runnerTmp := t.TempDir()
	runnerAlias := filepath.Join(t.TempDir(), "runner-alias")
	if err := os.Symlink(runnerTmp, runnerAlias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("TMPDIR", runnerAlias)
	for section, dir := range map[string]string{
		SessionDirs: sessionDir(deadSID),
		GoBuild:     "go-build-held",
		TmpDirs:     "tmp.held",
	} {
		t.Run(section, func(t *testing.T) {
			r, out := newRun(t, Options{Apply: true, Only: []string{section}})
			realTmp := r.TmpDir
			fixture(t, realTmp, filepath.Join(dir, "scratchpad", "keep"), 2048, 5*time.Hour)
			alias := filepath.Join(t.TempDir(), "tmp-alias")
			if err := os.Symlink(realTmp, alias); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			r.TmpDir = alias
			r.Scratch = filepath.Join(alias, "claude-1000")
			r.Table.ByPID[7] = &proc.Process{PID: 7, Comm: "tail"}
			// /proc resolves the alias, including for paths of deleted open files.
			r.holds = func(int) []string { return []string{filepath.Join(realTmp, dir, "open-but-deleted")} }
			if got := r.Free(); len(got) != 0 {
				t.Errorf("freed a held directory: %+v\n%s", got, out)
			}
			if _, err := os.Stat(filepath.Join(realTmp, dir, "scratchpad", "keep")); err != nil {
				t.Errorf("held content removed: %v\n%s", err, out)
			}
		})
	}
}
