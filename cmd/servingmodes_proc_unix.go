//go:build unix

package cmd

import (
	"os/exec"
	"syscall"
	"time"
)

// armServeModeSwitch makes the switch command its own process group and ends
// that whole group when the switch is cancelled: a switcher that runs its
// work in a child dies with it, and the machine's one-switch lock is never
// held past the timeout by an orphan.
func armServeModeSwitch(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	c.WaitDelay = 5 * time.Second
}
