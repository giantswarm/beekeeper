// Package check runs the external commands configured as checks: bounded by
// a timeout, stopped with SIGTERM so a command can close what it opened (a
// port-forward), never two runs of one check at once.
package check

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

// stopGrace is how long a command has after SIGTERM before it is killed.
const stopGrace = 15 * time.Second

func command(ctx context.Context, argv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the command is the person's own configuration
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = stopGrace
	cmd.Stdin = nil
	return cmd
}

// Snapshot runs the check's snapshot command and returns its output.
func Snapshot(ctx context.Context, c config.Check) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout.Duration)
	defer cancel()
	cmd := command(ctx, c.Snapshot)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return strings.TrimRight(out.String(), "\n"), failure(ctx, err, errb.String())
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// Watch runs the check's watch command every c.Every until ctx ends, passing
// each line it prints to line and each failed run to fail.
func Watch(ctx context.Context, c config.Check, line func(string), fail func(error)) {
	for {
		start := time.Now()
		if err := watchOnce(ctx, c, line); err != nil && ctx.Err() == nil {
			fail(err)
		}
		wait := time.Until(start.Add(c.Every.Duration))
		select {
		case <-ctx.Done():
			return
		case <-time.After(max(wait, 0)):
		}
	}
}

func watchOnce(ctx context.Context, c config.Check, line func(string)) error {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout.Duration)
	defer cancel()
	cmd := command(ctx, c.Watch)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if l := strings.TrimRight(sc.Text(), " \t"); l != "" {
			line(l)
		}
	}
	if err := cmd.Wait(); err != nil {
		return failure(ctx, err, errb.String())
	}
	return nil
}

func failure(ctx context.Context, err error, stderr string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("timed out")
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return fmt.Errorf("%w: %s", err, last)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ProcessState != nil {
		return fmt.Errorf("exit %d", ee.ExitCode())
	}
	return err
}
