//go:build unix

package cmd

import (
	"os/exec"
	"syscall"
)

// detach runs cmd in a session of its own: the end of its caller's session
// (SIGHUP, a signal to the process group) does not reach it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
