//go:build unix

package platform

import (
	"syscall"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// Detach runs cmd in a session of its own: the end of its caller's session
// (SIGHUP, a signal to the process group) does not reach it.
func Detach(cmd *proc.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
