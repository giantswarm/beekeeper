package guard

import (
	"os"

	worktreefs "github.com/giantswarm/beekeeper/internal/worktrees"
)

// TmpEnv are the temporary directory variables a run points off tmpfs:
// GOTMPDIR, where the go command writes its work directories (go-build*),
// and TMPDIR, every other tool's and a test's (t.TempDir).
var TmpEnv = []string{"GOTMPDIR", "TMPDIR"}

// runTmp makes the run's own temporary directory under base and returns
// the variables of TmpEnv that point into it and the cleanup that removes
// it after the command. A caller's variable that names a directory off
// tmpfs wins, so a nested run keeps its outer run's directory; with every
// variable the caller's, no directory is made. The disk step reclaims
// unused leftovers. A directory that cannot be made costs one line and
// the command runs with the caller's environment.
func runTmp(base string, logf func(string, ...any)) (env []string, cleanup func()) {
	cleanup = func() {}
	var unset []string
	for _, k := range TmpEnv {
		if v := os.Getenv(k); v == "" || onTmpfs(v) {
			unset = append(unset, k)
		}
	}
	if base == "" || len(unset) == 0 {
		return nil, cleanup
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		logf("temporary files stay in tmpfs: %v", err)
		return nil, cleanup
	}

	dir, err := os.MkdirTemp(base, "")
	if err != nil {
		logf("temporary files stay in tmpfs: %v", err)
		return nil, cleanup
	}
	for _, k := range unset {
		env = append(env, k+"="+dir)
	}
	return env, func() {
		if err := worktreefs.RemoveAll(dir); err != nil {
			logf("temporary directory cleanup failed: %v", err)
		}
	}
}
