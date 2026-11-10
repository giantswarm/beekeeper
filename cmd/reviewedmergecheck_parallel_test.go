package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/github"
)

// Model an occupied runner lock independently of the CI image's installed
// linter. A clean result must pass, but actual findings must still refuse merge.
func TestMergeResultLintWithAnotherRunner(t *testing.T) {
	for _, lintFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("findings=%v", lintFails), func(t *testing.T) {
			repo := newMergeCheckRepo(t, map[string]string{testGuardPR1File: testGuardSourceA}, map[string]string{testGuardPR2File: testGuardSourceB}, nil)
			lintDir := t.TempDir()
			script := `#!/bin/sh
parallel=false
for arg in "$@"; do
  if [ "$arg" = "--allow-parallel-runners" ]; then
    parallel=true
  fi
done
if [ "$parallel" != true ]; then
  echo 'Error: parallel golangci-lint is running'
  exit 1
fi
`
			if lintFails {
				script += "echo 'lint finding with another runner'\nexit 1\n"
			}
			if err := os.WriteFile(filepath.Join(lintDir, "golangci-lint"), []byte(script), 0o700); err != nil { //nolint:gosec // executable fixture in the test-owned directory
				t.Fatal(err)
			}
			t.Setenv("PATH", lintDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			reason, code := runReviewedMergeChecks(t.Context(), repo.dir, repo.base, mainBranch, repo.head2)
			if lintFails {
				if code != github.SquashRed || !strings.Contains(reason, "lint finding with another runner") {
					t.Fatalf("lint finding did not refuse merge: %d: %s", code, reason)
				}
			} else if code != 0 {
				t.Fatalf("clean tree refused with another runner: %d: %s", code, reason)
			}
		})
	}
}
