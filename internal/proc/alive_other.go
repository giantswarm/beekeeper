//go:build !unix

package proc

import (
	"io/fs"
	"os"
)

// Alive reports whether a process with that PID exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}

// ownerUID is unknown off unix.
func ownerUID(fs.FileInfo) int { return -1 }
