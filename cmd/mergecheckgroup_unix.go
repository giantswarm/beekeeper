//go:build unix && !linux

package cmd

import (
	"syscall"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// mergeCheckGroup runs a merge-result check in a process group of its own
// that its cancel ends whole.
func mergeCheckGroup(c *proc.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
}
