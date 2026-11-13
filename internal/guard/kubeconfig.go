package guard

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// runKubeconfig isolates default client-go writes. An inherited nonempty
// KUBECONFIG wins; assignments inside the command also override this default.
// An empty value needs isolation too: client-go treats it as unset.
func runKubeconfig() ([]string, func(), error) {
	if os.Getenv("KUBECONFIG") != "" {
		return nil, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "beekeeper-kubeconfig-")
	if err != nil {
		return nil, nil, err
	}
	return []string{"KUBECONFIG=" + filepath.Join(dir, "config")}, func() { _ = os.RemoveAll(dir) }, nil
}

// runDirect keeps a parent to clean up the scratch kubeconfig even when
// systemd is unavailable or this process already runs inside a cap.
func runDirect(argv, env []string, logf func(string, ...any)) int {
	c := proc.CommandContext(context.Background(), argv[0], argv[1:]...) //nolint:gosec // runs the caller's command
	c.Env = append(os.Environ(), env...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	if err := c.Start(); err != nil {
		logf("%v", err)
		return ExitNotFound
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	for {
		select {
		case s := <-sig:
			_ = c.Process.Signal(s)
		case err := <-done:
			if err == nil {
				return 0
			}
			if status, ok := c.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				return 128 + int(status.Signal())
			}
			return c.ProcessState.ExitCode()
		}
	}
}
