package proc

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestCmdOutputExitError checks that a failed command's error is this
// package's ExitError: its code, what it wrote on standard error, the
// state it shares with the command, and os/exec's error beneath.
//
// control: none: Cmd is new, the base has no such type
func TestCmdOutputExitError(t *testing.T) {
	c := CommandContext(context.Background(), "sh", "-c", "echo out; echo oops >&2; exit 3")
	out, err := c.Output()
	if string(out) != "out\n" {
		t.Errorf("output %q, want %q", out, "out\n")
	}
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("error %T %v, want an *ExitError", err, err)
	}
	if ee.ExitCode() != 3 || string(ee.Stderr) != "oops\n" || ee.Error() != "exit status 3" {
		t.Errorf("exit error: code %d, stderr %q, %q", ee.ExitCode(), ee.Stderr, ee.Error())
	}
	var osErr *exec.ExitError
	if !errors.As(err, &osErr) {
		t.Error("the exit error does not unwrap to os/exec's")
	}
	if c.ProcessState == nil || c.ProcessState.ExitCode() != 3 || ee.ProcessState != c.ProcessState {
		t.Errorf("command state %v, error state %v", c.ProcessState, ee.ProcessState)
	}
	if c.Process == nil || c.Process.Pid <= 0 {
		t.Errorf("no started process recorded: %+v", c.Process)
	}
}

// TestCmdStartWait checks that Start records the process os/exec started
// and Wait the state it exited with; a success is no error.
//
// control: none: Cmd is new, the base has no such type
func TestCmdStartWait(t *testing.T) {
	c := CommandContext(context.Background(), "sh", "-c", "exit 0")
	var stdout strings.Builder
	c.Stdout = &stdout
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if c.Process == nil || c.Process.Pid != c.Cmd.Process.Pid {
		t.Fatalf("Start recorded %+v, os/exec started %d", c.Process, c.Cmd.Process.Pid)
	}
	if c.ProcessState != nil {
		t.Fatal("a state before Wait")
	}
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	if c.ProcessState == nil || !c.ProcessState.Success() || c.ProcessState.Pid() != c.Process.Pid {
		t.Fatalf("Wait recorded %v", c.ProcessState)
	}
}

// TestCmdCancelSeesTheProcess checks that a Cancel of the command's own,
// which os/exec calls from its goroutine when the context ends, finds the
// started process to signal.
//
// control: none: Cmd is new, the base has no such type
func TestCmdCancelSeesTheProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := CommandContext(ctx, "sleep", "30")
	saw := make(chan *Child, 1)
	c.Cancel = func() error {
		saw <- c.Process
		return c.Process.Kill()
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()
	err := c.Wait()
	if p := <-saw; p == nil || p.Pid != c.Cmd.Process.Pid {
		t.Fatalf("Cancel saw %+v", p)
	}
	if err == nil || c.ProcessState == nil || c.ProcessState.Success() {
		t.Fatalf("a cancelled command: %v, state %v", err, c.ProcessState)
	}
}

// TestPrefixSuffixKeepsBothEnds checks the standard error an Output keeps:
// all of a short one, and the first and last part of a long one with the
// count of what lies between, as os/exec words it.
//
// control: none: prefixSuffix is new, the base has no such type
func TestPrefixSuffixKeepsBothEnds(t *testing.T) {
	short := &prefixSuffix{n: 4}
	_, _ = short.Write([]byte("abc"))
	_, _ = short.Write([]byte("de"))
	if got := string(short.Bytes()); got != "abcde" {
		t.Errorf("short: %q", got)
	}
	long := &prefixSuffix{n: 4}
	for _, s := range []string{"0123", "45", "6789ab", "cdef"} {
		if n, err := long.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if got, want := string(long.Bytes()), "0123\n... omitting 8 bytes ...\ncdef"; got != want {
		t.Errorf("long: %q, want %q", got, want)
	}
}

// TestCmdRunStartFailure checks that a program that does not exist fails
// Run with os/exec's lookup error, not an ExitError, and starts nothing.
//
// control: none: Cmd is new, the base has no such type
func TestCmdRunStartFailure(t *testing.T) {
	c := CommandContext(context.Background(), "beekeeper-test-no-such-program")
	err := c.Run()
	var ee *ExitError
	if !errors.Is(err, ErrNotFound) || errors.As(err, &ee) {
		t.Fatalf("Run = %v, want a lookup error", err)
	}
	if c.Process != nil || c.ProcessState != nil {
		t.Fatalf("process %+v, state %v after a failed start", c.Process, c.ProcessState)
	}
}
