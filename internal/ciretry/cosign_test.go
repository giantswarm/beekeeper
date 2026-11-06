package ciretry

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const signBlobVerb = "sign-blob"

func TestCosignRetry(t *testing.T) {
	for _, tc := range []struct {
		name, verb, failure   string
		failures, calls, code int
	}{
		{"success", signBlobVerb, "", 0, 1, 0},
		{"recover", signBlobVerb, "failed to fetch conflicting entry: getLogEntryByUuidNotFound", 2, 3, 0},
		{"exhausted", signBlobVerb, "getLogEntryByUuidNotFound", 9, 4, 7},
		{"permanent", signBlobVerb, "permission denied", 9, 1, 7},
		{"verification", "verify-blob", "getLogEntryByUuidNotFound", 9, 1, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stub := filepath.Join(dir, "cosign")
			script := `#!/usr/bin/env bash
set -eu
[[ "$#" == 4 && "$2" == --bundle && "$3" == 'bundle path' && "$4" == 'binary path' ]] || exit 99
count=0
[[ ! -f "$COUNT_FILE" ]] || read -r count < "$COUNT_FILE"
count=$((count + 1))
echo "$count" > "$COUNT_FILE"
if (( count <= FAILURES )); then
  echo "$FAILURE" >&2
  exit 7
fi
echo signed-output
echo success-diagnostic >&2
`
			if err := os.WriteFile(stub, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(stub, 0o700); err != nil { //nolint:gosec // executable test stub in a private temporary directory
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "../../.circleci/cosign-retry.sh", tc.verb, "--bundle", "bundle path", "binary path") //nolint:gosec // a path of this test
			cmd.Env = append(os.Environ(), "BASH_ENV=", "BEEKEEPER_COSIGN_BINARY="+stub, "BEEKEEPER_COSIGN_BACKOFF_SECONDS=0", "COUNT_FILE="+filepath.Join(dir, "count"), "FAILURE="+tc.failure, "FAILURES="+strconv.Itoa(tc.failures))
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					code = exit.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			count, readErr := os.ReadFile(filepath.Join(dir, "count")) //nolint:gosec // a path of this test
			if code != tc.code || readErr != nil || strings.TrimSpace(string(count)) != strconv.Itoa(tc.calls) {
				t.Fatalf("code=%d calls=%s read=%v output=%s", code, count, readErr, out)
			}
			if tc.code == 0 && (!strings.Contains(string(out), "signed-output") || !strings.Contains(string(out), "success-diagnostic")) {
				t.Fatalf("lost cosign output: %s", out)
			}
		})
	}
}

func TestInstallCosignRetry(t *testing.T) {
	dir := t.TempDir()
	// bash itself is the underlying probe: --version succeeds only if the
	// wrapper dispatches to the original binary instead of recursing.
	// command -v must resolve a real executable, so provide a symlink.
	probeDir := filepath.Join(dir, "probe")
	if err := os.Mkdir(probeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bash, filepath.Join(probeDir, "cosign")); err != nil {
		t.Fatal(err)
	}
	// Run the actual installation, then a later step of the same job.
	cmd := exec.Command("bash", "-eu", "-c", `bash .circleci/install-cosign-retry.sh
source "$BASH_ENV"
test "$(command -v cosign)" = "$EXPECTED_WRAPPER"
cosign --version
`)
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(), "BASH_ENV="+filepath.Join(dir, "env"), "BEEKEEPER_COSIGN_BIN_DIR="+filepath.Join(dir, "bin"), "PATH="+probeDir+":"+os.Getenv("PATH"), "EXPECTED_WRAPPER="+filepath.Join(dir, "bin", "cosign"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install and dispatch: %v: %s", err, out)
	}
}

// The retry reaches the signing step only through the generated job's make
// test: a generation that stops running it there must turn this test red.
func TestGoBuildRunsMakeTest(t *testing.T) {
	raw, err := os.ReadFile("../../.circleci/workflows.yml")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Workflows map[string]struct {
			Jobs []map[string]struct {
				Name       string `yaml:"name"`
				TestTarget string `yaml:"test_target"`
			}
		}
	}
	if err := yaml.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	for _, job := range config.Workflows["build"].Jobs {
		build, ok := job["architect/go-build"]
		if !ok {
			continue
		}
		if build.Name != "go-build" || build.TestTarget != "test" {
			t.Fatalf("go-build job %+v no longer installs the retry", build)
		}
		return
	}
	t.Fatal("missing architect/go-build job")
}

func TestMakeTestInstallsCosignRetryOnlyInGoBuild(t *testing.T) {
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make is not installed")
	}
	for job, want := range map[string]bool{"go-build": true, "test-stable-version": false} {
		t.Run(job, func(t *testing.T) {
			cmd := exec.Command(makeBin, "-n", "test", "CIRCLE_JOB="+job) //nolint:gosec // the job names of this test
			cmd.Dir = "../.."
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("make: %v\n%s", err, out)
			}
			if got := strings.Contains(string(out), "install-cosign-retry.sh"); got != want {
				t.Fatalf("installs the retry: %v, want %v\n%s", got, want, out)
			}
		})
	}
}
