package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sync"
	"time"
)

// Cmd is a process beekeeper starts. It is configured as exec.Cmd is (Path,
// Args, Env, Dir, the standard streams and their pipes, ExtraFiles,
// SysProcAttr, Cancel, WaitDelay), but the process it starts and the state
// that process exits with are this package's types, and so are the errors
// of an exit. Every process the core starts goes through this type, so that
// one seam can answer them all: a release build answers with os/exec and
// nothing else, and the behavioral scenarios will answer in their test
// process (docs/plans/3788-behavioral-scenarios.md, the process seam).
type Cmd struct {
	*exec.Cmd
	// Process is the started process; nil before Start, and for a Cancel
	// already when it runs.
	Process *Child
	// ProcessState is the exited process's state; nil before Wait.
	ProcessState *ProcessState

	start sync.Once
}

// CommandContext is exec.CommandContext: the command name with args, killed
// when ctx is done before it exits.
func CommandContext(ctx context.Context, name string, arg ...string) *Cmd {
	return &Cmd{Cmd: exec.CommandContext(ctx, name, arg...)} // #nosec G204 -- the seam every core start goes through; each caller names its program
}

// Start starts the command without waiting for it. A Cancel of the
// command's own runs with Process set: os/exec may call it from its own
// goroutine before Start has returned.
func (c *Cmd) Start() error {
	if cancel := c.Cmd.Cancel; cancel != nil {
		c.Cmd.Cancel = func() error {
			c.started()
			return cancel()
		}
	}
	err := c.Cmd.Start()
	c.started()
	return err
}

// Wait waits for the started command to exit. An exit with a status other
// than 0 is an *ExitError.
func (c *Cmd) Wait() error {
	err := c.Cmd.Wait()
	c.exited()
	return c.exitError(err)
}

// Run starts the command and waits for it.
func (c *Cmd) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

// Output runs the command and returns its standard output. Without a
// Stderr of its own, an *ExitError carries what it wrote there: the first
// and the last 32 KiB, as os/exec keeps them. It runs through Start and
// Wait, so Process is set while the command runs, for a Cancel that
// signals it.
func (c *Cmd) Output() ([]byte, error) {
	if c.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	var stdout bytes.Buffer
	c.Stdout = &stdout
	var stderr *prefixSuffix
	if c.Stderr == nil {
		stderr = &prefixSuffix{n: 32 << 10}
		c.Stderr = stderr
	}
	err := c.Run()
	var ee *ExitError
	if stderr != nil && errors.As(err, &ee) {
		ee.Stderr = stderr.Bytes()
	}
	return stdout.Bytes(), err
}

// CombinedOutput runs the command and returns its standard output and
// standard error together, through Start and Wait as Output does.
func (c *Cmd) CombinedOutput() ([]byte, error) {
	if c.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	if c.Stderr != nil {
		return nil, errors.New("exec: Stderr already set")
	}
	var b bytes.Buffer
	c.Stdout, c.Stderr = &b, &b
	err := c.Run()
	return b.Bytes(), err
}

// prefixSuffix keeps the first n and the last n bytes written to it, as
// os/exec keeps an Output's standard error.
type prefixSuffix struct {
	n       int
	prefix  []byte
	suffix  []byte
	skipped int64
}

func (w *prefixSuffix) Write(p []byte) (int, error) {
	written := len(p)
	if room := w.n - len(w.prefix); room > 0 {
		k := min(room, len(p))
		w.prefix = append(w.prefix, p[:k]...)
		p = p[k:]
	}
	w.suffix = append(w.suffix, p...)
	if over := len(w.suffix) - w.n; over > 0 {
		w.skipped += int64(over)
		w.suffix = append(w.suffix[:0], w.suffix[over:]...)
	}
	return written, nil
}

// Bytes is what was kept, with the count of the bytes left out between the
// first and the last part.
func (w *prefixSuffix) Bytes() []byte {
	if w.skipped == 0 {
		return slices.Concat(w.prefix, w.suffix)
	}
	return slices.Concat(w.prefix, fmt.Appendf(nil, "\n... omitting %d bytes ...\n", w.skipped), w.suffix)
}

// started takes over the process os/exec started, once: from Start, or
// from a Cancel that runs first.
func (c *Cmd) started() {
	c.start.Do(func() {
		if p := c.Cmd.Process; p != nil {
			c.Process = &Child{Pid: p.Pid, os: p}
		}
	})
}

// exited takes over the state os/exec recorded.
func (c *Cmd) exited() {
	if s := c.Cmd.ProcessState; s != nil {
		c.ProcessState = &ProcessState{os: s}
	}
}

// exitError is err, os/exec's *exec.ExitError in this package's ExitError,
// which unwraps to it.
func (c *Cmd) exitError(err error) error {
	ee, ok := err.(*exec.ExitError) //nolint:errorlint // os/exec returns it as it is
	if !ok {
		return err
	}
	state := c.ProcessState
	if state == nil || state.os != ee.ProcessState {
		state = &ProcessState{os: ee.ProcessState}
	}
	return &ExitError{ProcessState: state, Stderr: ee.Stderr, err: ee}
}

// Child is a process a Cmd started.
type Child struct {
	Pid int
	os  *os.Process
}

// Signal sends sig to the process.
func (p *Child) Signal(sig os.Signal) error { return p.os.Signal(sig) }

// Kill ends the process at once.
func (p *Child) Kill() error { return p.os.Kill() }

// Release frees what the process holds without waiting for it.
func (p *Child) Release() error { return p.os.Release() }

// ProcessState is how a process exited.
type ProcessState struct {
	os *os.ProcessState
}

// ExitCode is the exit code, or -1 for a process a signal ended or one that
// has not exited.
func (s *ProcessState) ExitCode() int { return s.os.ExitCode() }

// Exited says whether the process exited by itself.
func (s *ProcessState) Exited() bool { return s.os.Exited() }

// Success says whether the process exited with status 0.
func (s *ProcessState) Success() bool { return s.os.Success() }

// Pid is the exited process's id.
func (s *ProcessState) Pid() int { return s.os.Pid() }

// Sys is the system's exit information, a syscall.WaitStatus on Unix.
func (s *ProcessState) Sys() any { return s.os.Sys() }

// SysUsage is the system's resource usage, a *syscall.Rusage on Unix.
func (s *ProcessState) SysUsage() any { return s.os.SysUsage() }

// SystemTime is the CPU time the process spent in the system.
func (s *ProcessState) SystemTime() time.Duration { return s.os.SystemTime() }

// UserTime is the CPU time the process spent on its own.
func (s *ProcessState) UserTime() time.Duration { return s.os.UserTime() }

// String describes the exit as os.ProcessState does ("exit status 1").
func (s *ProcessState) String() string { return s.os.String() }

// ExitError is a command that exited with a status other than 0.
type ExitError struct {
	*ProcessState
	// Stderr is what the command wrote on standard error, for Output
	// without a Stderr of its own.
	Stderr []byte
	err    error
}

func (e *ExitError) Error() string { return e.err.Error() }

// Unwrap is the operating system's error, an *exec.ExitError.
func (e *ExitError) Unwrap() error { return e.err }

// ErrNotFound is the error of a program PATH does not have.
var ErrNotFound = exec.ErrNotFound

// LookPath is exec.LookPath: the path of the program file, searched in the
// directories of PATH for a bare name. LookTool also searches the Go tool
// directories.
func LookPath(file string) (string, error) { return exec.LookPath(file) }
