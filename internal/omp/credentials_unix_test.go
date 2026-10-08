//go:build unix

package omp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The unit's shell reads one NAME=value line per credential from the inbox
// before it execs omp, which then has them in its environment and the
// inbox, with what follows the lines, on its stdin.
func TestShellArgvHandsTheCredentialsOver(t *testing.T) {
	const planted = "planted-key-3a7c" //nolint:gosec // a planted test value
	dir := t.TempDir()
	inbox := InboxPath(dir, "1234abcd")
	if err := MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	// omp's stand-in: what it got, and the first line of its stdin
	argv := ShellArgv(inbox, []string{keyVar, otherKeyVar}, "/bin/sh", "-c", `IFS= read -r first; printf '%s|%s|%s' "$A_KEY" "$B_KEY" "$first" >"$1"`, "omp", out)
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // the test's own command line
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w, err := os.OpenFile(inbox, os.O_WRONLY, 0) //nolint:gosec // the test's inbox
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("A_KEY=" + planted + "\nB_KEY=second value\n"); err != nil {
		t.Fatal(err)
	}
	if err := Send(inbox, "hello"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the shell: %v", err)
	}
	raw, err := os.ReadFile(out) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); !strings.HasPrefix(got, planted+"|second value|") || !strings.Contains(got, `"message":"hello"`) {
		t.Errorf("omp got %q", got)
	}
}

// A line that is not the named credential ends the unit instead of running
// omp on a key it does not have.
func TestShellArgvRefusesAnotherLine(t *testing.T) {
	dir := t.TempDir()
	inbox := InboxPath(dir, "1234abcd")
	if err := MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	argv := ShellArgv(inbox, []string{keyVar}, "/bin/sh", "-c", "exit 0")
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // the test's own command line
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w, err := os.OpenFile(inbox, os.O_WRONLY, 0) //nolint:gosec // the test's inbox
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(`{"type":"steer","message":"too early"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	var ee *exec.ExitError
	if err := cmd.Wait(); err == nil || !errorsAs(err, &ee) || ee.ExitCode() != 78 {
		t.Fatalf("the shell ended with %v, want exit 78", err)
	}
}

// Without credentials the shell execs omp at once on the inbox.
func TestShellArgvWithoutCredentials(t *testing.T) {
	dir := t.TempDir()
	inbox := InboxPath(dir, "1234abcd")
	if err := MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	argv := ShellArgv(inbox, nil, "/bin/sh", "-c", `IFS= read -r first; printf '%s' "$first" >"$1"`, "omp", out)
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // the test's own command line
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for {
		if err := Send(inbox, "hello"); err == nil {
			break
		} else if err != ErrNotRunning { //nolint:errorlint // the sentinel itself
			t.Fatal(err)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(out); !strings.Contains(string(raw), `"message":"hello"`) { //nolint:gosec // the test's own file
		t.Errorf("omp got %q", raw)
	}
}

// The tool shell drops the credentials omp passes on with its environment,
// and runs bash with the arguments omp gives it.
func TestToolShellDropsTheCredentials(t *testing.T) {
	const planted = "planted-key-5e1d" //nolint:gosec // a planted test value
	shell, err := WriteToolShell(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(shell, "-c", `printf '%s|%s|%s' "${A_KEY-unset}" "${B_KEY-unset}" "$KEPT"`) //nolint:gosec // the test's own shell
	cmd.Env = []string{"PATH=/usr/bin:/bin", keyVar + "=" + planted, otherKeyVar + "=x", "KEPT=kept", EnvCredentials + "=" + keyVar + " " + otherKeyVar}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "unset|unset|kept" {
		t.Errorf("the tool shell saw %q", out)
	}
}

func errorsAs(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError) //nolint:errorlint // exec.Command's own error
	if ok {
		*target = ee
	}
	return ok
}
