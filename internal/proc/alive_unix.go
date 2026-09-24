//go:build unix

package proc

import (
	"errors"
	"syscall"
)

// Alive reports whether a process with that PID exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
