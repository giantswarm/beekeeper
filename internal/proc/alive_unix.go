//go:build unix

package proc

import (
	"errors"
	"io/fs"
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

// ownerUID is the user that owns a /proc/<pid> entry: the process's.
func ownerUID(info fs.FileInfo) int {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}
