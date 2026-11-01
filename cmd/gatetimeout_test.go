//go:build linux

package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A gated release wait whose caller's timeout kills the call runs on: the
// call ends at once with exit 76 and the line that the timeout cut the
// connection, not the wait; devctl's verdict wakes the owner once it ends.
func TestACallersTimeoutCutsTheCallNotTheWait(t *testing.T) {
	// coreutils' timeout runs the command as its child; busybox's execs it
	// and times it from a child of its own, no ancestor the gate can see.
	timeout, err := exec.LookPath(timeoutComm)
	if err != nil {
		t.Skip("no timeout")
	}
	if v, _ := exec.Command(timeout, "--version").Output(); !strings.Contains(string(v), "coreutils") { //nolint:gosec // the machine's timeout
		t.Skip("timeout is not coreutils'")
	}
	if runningBinary().Path() == "" {
		t.Skip("the running binary cannot be read here")
	}
	if _, err := plat.Machine.Processes(); err != nil {
		t.Skipf("no process table on this platform, the gate cannot tell its caller: %v", err)
	}
	dir := t.TempDir()
	// A PATH of its own without systemd-run: merge-child runs in a session
	// of its own and inherits the run's environment, the wakes file with it.
	bin := filepath.Join(dir, "path")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"sh", "sleep"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s", tool)
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	const doc = `{"verdict":"available","reason":"v1.2.3 is pullable"}`
	proceed := filepath.Join(dir, "proceed")
	devctl := `#!/bin/sh
echo "waiting for pipeline 1538 unfinished" >&2
while [ ! -e ` + proceed + ` ]; do sleep 0.05; done
echo '` + doc + `'
`
	if err := os.WriteFile(filepath.Join(bin, "devctl"), []byte(devctl), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(proceed, nil, 0o600) })
	stateHome := filepath.Join(dir, "state")
	store, err := state.Open(filepath.Join(stateHome, "beekeeper"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Session: "s1", Name: ownerName}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(dir, "beekeeper")
	copyTestBinary(t, self)
	wakes := filepath.Join(dir, "wakes")
	call := exec.Command(timeout, "2", self, gateCmdName, "--", "devctl", "release", "wait", "o/r", "v1.2.3") //nolint:gosec // this test binary's copy
	call.Env = append(os.Environ(), "BEEKEEPER_TEST_MAIN=1", testWakesEnv+"="+wakes, "PATH="+bin,
		"BEEKEEPER_CONFIG="+emptyTestConfig(t, filepath.Join(dir, "config.yaml")), "XDG_STATE_HOME="+stateHome,
		"CLAUDE_CODE_SESSION_ID=s1", "CLAUDE_CODE_HOST_SESSION_ID=", "CLAUDE_CODE_SESSION_NAME="+ownerName)
	stderr := &lines{}
	call.Stderr = stderr
	started := time.Now()
	err = call.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 124 {
		t.Fatalf("the caller's timeout: %v, want its exit 124\n%s", err, stderr)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("the call took %s past its timeout of 2s", took)
	}
	said := stderr.String()
	for _, want := range []string{
		"SIGTERM from the caller's timeout: it cut the connection, not the wait",
		`devctl release wait o/r v1.2.3 runs on outside the call`,
		`its outcome wakes "worker": do not run it again, do not poll`,
	} {
		if !strings.Contains(said, want) {
			t.Errorf("the gate did not say %q:\n%s", want, said)
		}
	}
	if _, err := os.Stat(wakes); err == nil {
		t.Fatalf("the owner was woken while the wait ran")
	}

	if err := os.WriteFile(proceed, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var woke string
	for deadline := time.Now().Add(30 * time.Second); woke == "" && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		raw, _ := os.ReadFile(wakes) //nolint:gosec // the test's file
		woke = string(raw)
	}
	want := "beekeeper gate → s1: devctl release wait o/r v1.2.3 exit 0: available: v1.2.3 is pullable (output in "
	if !strings.HasPrefix(woke, want) || strings.Count(woke, "\n") != 1 {
		t.Errorf("woke %q, want one line starting %q", woke, want)
	}
	evs, err := store.Events(0, func(e state.Event) bool { return e.Verb == verbUnheard })
	if err != nil || len(evs) != 1 || !strings.HasSuffix(evs[0].Detail, `; waking "worker"`) {
		t.Errorf("devctl.unheard events %+v (%v)", evs, err)
	}
}

// A timeout counts as the caller's below the Claude Code CLI only: one the
// CLI itself runs under is no caller of the gate.
func TestUnderTimeout(t *testing.T) {
	// table is the gate, pid 1, below its ancestors' comms, nearest first.
	table := func(comms ...string) *proc.Table {
		tb := &proc.Table{ByPID: map[int]*proc.Process{1: {PID: 1, PPID: 2, Comm: gateCmdName}}}
		for i, c := range comms {
			tb.ByPID[i+2] = &proc.Process{PID: i + 2, PPID: i + 3, Comm: c}
		}
		return tb
	}
	for _, c := range []struct {
		comms []string
		want  bool
	}{
		{[]string{timeoutComm, "sh", claudeComm}, true},
		{[]string{"sh", timeoutComm, claudeComm}, true},
		{[]string{"sh", claudeComm, timeoutComm}, false},
		{[]string{"sh"}, false},
	} {
		if got := underTimeout(table(c.comms...), 1); got != c.want {
			t.Errorf("%v: %t, want %t", c.comms, got, c.want)
		}
	}
}
