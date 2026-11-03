//go:build unix && !linux

package cmd

import (
	"os/exec"
	"syscall"
)

// mergeCheckGroup runs a merge-result check in a process group of its own
// that its cancel ends whole.
func mergeCheckGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
}
