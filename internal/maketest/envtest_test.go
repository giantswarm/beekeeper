package maketest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Exercise the real makefiles with tool doubles: envtests must not depend on
// version discovery or Postgres, and a failed asset download must fail make.
func TestEnvtestTarget(t *testing.T) {
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make is not installed")
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	for _, failAssets := range []bool{false, true} {
		name := "without database"
		if failAssets {
			name = "asset download fails"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{"Makefile", "Makefile.custom.mk", "Makefile.gen.go.mk", "Makefile.gen.app.mk"} {
				data, err := os.ReadFile(filepath.Join(root, name)) //nolint:gosec // a path of this test
				if err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(dir, name), string(data), 0o600)
			}
			// A linked worktree has a .git file, not a directory. The version
			// tool deliberately fails if make calls it for this target.
			write(t, filepath.Join(dir, ".git"), "gitdir: unused\n", 0o600)
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(bin, "gitsemver"), "#!/bin/sh\necho unexpected-version-lookup >&2\nexit 1\n", 0o700)
			write(t, filepath.Join(bin, "git"), "#!/bin/sh\necho fixture\n", 0o700)
			write(t, filepath.Join(bin, "go"), `#!/bin/sh
case "$1" in
list) echo github.com/giantswarm/beekeeper ;;
env) echo linux ;;
run)
  test "$FAIL_ASSETS" != 1 || exit 1
  echo /envtest-assets ;;
test)
  test -z "$BEEKEEPER_TEST_DATABASE_URL" || exit 2
  test "$KUBEBUILDER_ASSETS" = /envtest-assets || exit 3
  echo envtests-executed ;;
*) exit 4 ;;
esac
`, 0o700)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("BEEKEEPER_TEST_DATABASE_URL", "")
			t.Setenv("FAIL_ASSETS", "0")
			if failAssets {
				t.Setenv("FAIL_ASSETS", "1")
			}
			c := exec.Command(makeBin, "test-envtest")
			c.Dir = dir
			out, err := c.CombinedOutput()
			if (err != nil) != failAssets {
				t.Fatalf("make: %v\n%s", err, out)
			}
			if strings.Contains(string(out), "unexpected-version-lookup") {
				t.Fatalf("envtests invoked gitsemver:\n%s", out)
			}
			if strings.Contains(string(out), "\nenvtests-executed\n") == failAssets {
				t.Fatalf("unexpected test execution:\n%s", out)
			}
		})
	}
}

func write(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), mode); err != nil { //nolint:gosec // a path of this test
		t.Fatal(err)
	}
}
