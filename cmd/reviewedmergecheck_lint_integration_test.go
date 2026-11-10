package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/github"
)

// CI opts in with a real golangci-lint on PATH. The rejecting control ensures
// that a missing, disabled or stubbed linter cannot make this proof pass.
func TestMergeResultRealLintIgnoreTests(t *testing.T) {
	if os.Getenv("BEEKEEPER_TEST_REAL_LINT") != "1" {
		t.Skip("set BEEKEEPER_TEST_REAL_LINT=1 with golangci-lint installed")
	}
	if _, err := exec.LookPath("golangci-lint"); err != nil {
		t.Fatal(err)
	}
	for _, ignoreTests := range []bool{true, false} {
		t.Run(fmt.Sprintf("ignore-tests=%v", ignoreTests), func(t *testing.T) {
			dir := t.TempDir()
			files := map[string]string{
				"go.mod":   "module example.com/lintproof\n\ngo 1.24\n",
				"guard.go": "package guard\n",
				"guard_test.go": `package guard

func repeatedLiteral(n int) string {
	if n == 1 {
		return "repeated-policy"
	}
	if n == 2 {
		return "repeated-policy"
	}
	return "repeated-policy"
}
`,
				".golangci.yml": fmt.Sprintf(`version: "2"
linters:
  default: none
  enable:
    - goconst
  settings:
    goconst:
      ignore-tests: %v
`, ignoreTests),
			}
			for name, content := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			reason, code := runReviewedMergeChecks(t.Context(), dir, "", mainBranch, "1234567890abcdef")
			if ignoreTests {
				if code != 0 {
					t.Fatalf("ignored test literals refused: %d: %s", code, reason)
				}
				return
			}
			if code != github.SquashRed || !strings.Contains(reason, "goconst") || !strings.Contains(reason, "guard_test.go") || !strings.Contains(reason, "config: .golangci.yml") {
				t.Fatalf("expected configured goconst refusal for test literals: %d: %s", code, reason)
			}
		})
	}
}
