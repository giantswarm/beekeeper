package alerts

import (
	"os/exec"
	"syscall"
)

// ownGroup runs cmd in a process group of its own that its cancel ends with
// SIGTERM, and that the kernel ends when beekeeper itself dies.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
}
