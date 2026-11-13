package cmd

import (
	"syscall"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// mergeCheckGroup runs a merge-result check (golangci-lint, go test) in a
// process group of its own that its cancel ends whole, the compilers and
// test binaries it started too, and whose leader the kernel ends when
// beekeeper itself dies: no check outlives the merge that ran it.
func mergeCheckGroup(c *proc.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
}
