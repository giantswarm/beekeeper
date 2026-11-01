package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/proc"
)

func TestRunnerUsesOwnedConversationAndCleanEnvironment(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("execution runner uses a Linux user service")
	}
	tests := []struct {
		name, conversation string
		ready              bool
	}{
		{name: "accepted first task", ready: true},
		{name: "foreign conversation", conversation: "foreign", ready: false},
		{name: "ended before acceptance", conversation: "missing", ready: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := testFixture(t)
			ctx := context.Background()
			child := filepath.Join(t.TempDir(), "fake-harness")
			script := `#!/bin/sh
set -eu
session=''
while [ "$#" -gt 0 ]; do
 if [ "$1" = '--session-id' ]; then session="$2"; shift; fi
 shift
done
test -z "${CLAUDE_CODE_SESSION_ID+x}"
test -z "${ANTHROPIC_API_KEY+x}"
test -n "${BEEKEEPER_EXECUTION_ID}"
test -n "${BEEKEEPER_CONFIG}"
input=$(cat)
test "$input" = 'complete the fixture task'
`
			switch test.conversation {
			case "foreign":
				script += "printf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"foreign\"}'\n"
			case "missing":
				script += "printf '%s\\n' '{\"type\":\"result\"}'\n"
			default:
				script += "printf '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"%s\"}\\n' \"$session\"\n"
			}
			if err := os.WriteFile(child, []byte(script), 0o700); err != nil { //nolint:gosec // executable fake harness in the test's own temporary directory
				t.Fatal(err)
			}
			h := f.s.Config.Harnesses[HarnessClaude]
			h.Executable = child
			f.s.Config.Harnesses[HarnessClaude] = h
			original := f.s.Command
			f.s.Command = func(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
				if bin == child {
					bin = fixtureClaudeExecutable
				}
				return original(ctx, dir, env, bin, args...)
			}
			digest, err := f.s.Prepare(f.s.Policy())
			if err != nil {
				t.Fatal(err)
			}
			f.r.PolicyDigest = digest
			t.Setenv("CLAUDE_CODE_SESSION_ID", "fixture-parent")
			t.Setenv("ANTHROPIC_API_KEY", "fixture-value")
			if _, err := f.s.Start(ctx, f.r); err != nil {
				t.Fatal(err)
			}
			f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
			if _, err := f.s.Start(ctx, f.r); err != nil {
				t.Fatal(err)
			}
			err = f.s.Run(ctx, f.r.Ref)
			if (err == nil) != test.ready {
				t.Fatalf("unexpected first-task result: %v", err)
			}
			j, err := f.s.Read(f.r.Ref)
			if err != nil {
				t.Fatal(err)
			}
			if j.Ready != test.ready {
				t.Fatal("false first-task acceptance")
			}
			if j.RunnerPID != os.Getpid() || j.RunnerStarted.IsZero() {
				t.Fatal("runner identity not journaled")
			}
			if test.ready && j.Phase != Exited {
				t.Fatal("completed fake harness did not become terminal")
			}
			if err := f.s.Run(ctx, f.r.Ref); err == nil {
				t.Fatal("runner launched twice")
			}
		})
	}
}

func TestCallerPreservesIdentityAcrossLoginShell(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("execution identity uses Linux process ancestry")
	}
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell unavailable")
	}
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	started, err := proc.Started(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error { j.RunnerPID = os.Getpid(); j.RunnerStarted = started; return nil }); err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, shell, "-lc", `exec "$1" -test.run=^TestExecutionCallerHelper$`, "fixture-shell", os.Args[0]) //nolint:gosec // runs only this test binary through a local login shell
	child.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin", "BEEKEEPER_EXECUTION_TEST_ROOT=" + f.s.Root, "BEEKEEPER_EXECUTION_TEST_ID=" + string(f.r.Ref.Execution)}
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("login shell lost execution identity: %v, %s", err, output)
	}
	// A reused PID is not sufficient: the recorded process start must match.
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error { j.RunnerStarted = j.RunnerStarted.Add(-1); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Caller(string(f.r.Ref.Execution)); err == nil {
		t.Fatal("stale runner identity was accepted")
	}
}

func TestExecutionCallerHelper(t *testing.T) {
	root := os.Getenv("BEEKEEPER_EXECUTION_TEST_ROOT")
	if root == "" {
		return
	}
	s := &Server{Root: root, Config: HostConfig{Identity: fixtureHost}, OwnUnit: func() string { return "fixture-build.scope" }}
	j, err := s.Caller(os.Getenv("BEEKEEPER_EXECUTION_TEST_ID"))
	if err != nil {
		t.Fatal(err)
	}
	if j.Request.Name != fixtureWorker || j.Request.Coordinator != fixtureCoordinator {
		t.Fatal("incorrect worker identity after login shell")
	}
}

func TestRunnerPinsWorkerContractToInstalledExecutable(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("execution runner uses a Linux user service")
	}
	// A stale copy elsewhere on the inherited PATH must not shadow the
	// policy-bound executable the managed worker commands resolve to.
	shadow := t.TempDir()
	if err := os.WriteFile(filepath.Join(shadow, "beekeeper"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // fixture executable in the test's own temporary directory
		t.Fatal(err)
	}
	t.Setenv("PATH", shadow+string(filepath.ListSeparator)+"/usr/bin:/bin")
	f := testFixture(t)
	ctx := context.Background()
	child := filepath.Join(t.TempDir(), "fake-harness")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
session=''
while [ "$#" -gt 0 ]; do
 if [ "$1" = '--session-id' ]; then session="$2"; shift; fi
 shift
done
test "$(command -v beekeeper)" = '%s'
input=$(cat)
test "$input" = 'complete the fixture task'
printf '{"type":"system","subtype":"init","session_id":"%%s"}\n' "$session"
`, f.s.Executable)
	if err := os.WriteFile(child, []byte(script), 0o700); err != nil { //nolint:gosec // executable fake harness in the test's own temporary directory
		t.Fatal(err)
	}
	h := f.s.Config.Harnesses[HarnessClaude]
	h.Executable = child
	f.s.Config.Harnesses[HarnessClaude] = h
	original := f.s.Command
	f.s.Command = func(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
		if bin == child {
			bin = fixtureClaudeExecutable
		}
		return original(ctx, dir, env, bin, args...)
	}
	digest, err := f.s.Prepare(f.s.Policy())
	if err != nil {
		t.Fatal(err)
	}
	f.r.PolicyDigest = digest
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Run(ctx, f.r.Ref); err != nil {
		t.Fatalf("worker contract could not resolve the installed executable through the harness PATH: %v", err)
	}
}

func TestCleanEnvironmentPinsComponentDirectory(t *testing.T) {
	t.Setenv("PATH", "/usr/local/bin:/usr/bin")
	component := filepath.Join(t.TempDir(), "per", "user", ".local", "bin", "beekeeper")
	env := cleanEnvironment("/srv/auth", component)
	var path string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	dirs := filepath.SplitList(path)
	if len(dirs) == 0 || dirs[0] != filepath.Dir(component) {
		t.Fatalf("component directory is not first on PATH: %q", path)
	}
	count := 0
	for _, d := range dirs {
		if d == filepath.Dir(component) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("component directory appears %d times on PATH: %q", count, path)
	}
	t.Setenv("PATH", "")
	env = cleanEnvironment("", component)
	if !slices.Contains(env, "PATH="+filepath.Dir(component)) {
		t.Fatalf("empty PATH gained no component directory: %v", env)
	}
	if got := filepath.SplitList(pathWithFirst("/a", "/a:/a:/b")); len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Fatalf("pathWithFirst kept duplicates: %v", got)
	}
}

func TestCheckRefusesUnresolvableWorkerContract(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	tr := f.transport(t, nil)
	if _, err := tr.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.s.Executable, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Check(ctx); err == nil || !strings.Contains(err.Error(), "no executable beekeeper") {
		t.Fatalf("readiness accepted a worker contract that cannot execute: %v", err)
	}
	if err := os.Remove(f.s.Executable); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Check(ctx); err == nil {
		t.Fatal("readiness accepted a missing installation")
	}
	if _, err := tr.Start(ctx, f.r); err == nil || len(f.launcher.starts) != 0 {
		t.Fatal("start launched a worker that cannot report or park")
	}
}

func TestCheckRefusesShadowedBoundExecutable(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	dir := filepath.Dir(f.s.Executable)
	bound := filepath.Join(dir, "beekeeper-v2")
	if err := os.Rename(f.s.Executable, bound); err != nil {
		t.Fatal(err)
	}
	// An older sibling under the command name the worker contract invokes:
	// the runner prepends this directory, so a bare `beekeeper` would reach
	// it instead of the executable the prepared policy binds.
	if err := os.WriteFile(filepath.Join(dir, "beekeeper"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // stale sibling fixture in the test's own temporary directory
		t.Fatal(err)
	}
	f.s.Executable = bound
	digest, err := f.s.Prepare(f.s.Policy())
	if err != nil {
		t.Fatal(err)
	}
	f.r.PolicyDigest = digest
	tr := f.transport(t, nil)
	tr.Host.PolicyDigest = digest
	if _, err := tr.Check(ctx); err == nil || !strings.Contains(err.Error(), "does not resolve to the bound execution host executable") {
		t.Fatalf("readiness accepted a differently named bound binary shadowed by a stale sibling: %v", err)
	}
	if _, err := tr.Start(ctx, f.r); err == nil || len(f.launcher.starts) != 0 {
		t.Fatal("start launched a worker whose bare commands reach a different executable")
	}
	// With the bound binary installed under the worker command name, the
	// same host is ready again.
	if err := os.Remove(filepath.Join(dir, "beekeeper")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(bound, filepath.Join(dir, "beekeeper")); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Check(ctx); err != nil {
		t.Fatalf("host with the bound installation under the worker command name refused: %v", err)
	}
}

func TestRunnerRechecksLiveCoordinatorBeforeProcess(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	f.s.Authorize = func(context.Context, StartRequest) error {
		return errors.New("fixture stop between activation and launch")
	}
	if err := f.s.Run(ctx, f.r.Ref); err == nil || !strings.Contains(err.Error(), "fixture stop") {
		t.Fatal("runner ignored revoked authority")
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if j.Ready || j.RunnerPID != 0 {
		t.Fatalf("revoked execution accepted a task: %v", j.Ready)
	}
}
