//go:build linux

package platform

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

// Binary is the executable a gate call runs. beekeeper self-update renames a
// new binary over its path; a waiting call notices the other file there and
// re-executes it, so a fix reaches the calls that already wait.
type Binary struct {
	Path string
	// self is the running file, which /proc/self/exe keeps reaching after
	// the Path names another.
	self os.FileInfo
}

// RunningBinary is this process's executable, nil when /proc cannot say.
func RunningBinary() *Binary { return ProcessBinary(os.Getpid()) }

// ProcessBinary is the executable process pid runs, nil when /proc cannot
// say: its path, and the file it runs, which stays reachable at
// /proc/<pid>/exe after the path names another.
func ProcessBinary(pid int) *Binary {
	exe := fmt.Sprintf("/proc/%d/exe", pid)
	path, err := os.Readlink(exe)
	if err != nil {
		return nil
	}
	self, err := os.Stat(exe)
	if err != nil {
		return nil
	}
	// The link of a file renamed over or removed names its path with this.
	return &Binary{Path: strings.TrimSuffix(path, " (deleted)"), self: self}
}

// Replaced says whether the path now names another executable file than
// the running one.
func (b *Binary) Replaced() bool {
	if b == nil {
		return false
	}
	fi, err := os.Stat(b.Path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 && !os.SameFile(fi, b.self)
}

// Exec replaces this process with the file at the path: the same pid,
// argument vector, stdio and environment plus env. It returns only when the
// new binary does not start; the call then stays on the running one until
// another file replaces it.
func (b *Binary) Exec(env ...string) error {
	err := syscall.Exec(b.Path, os.Args, append(os.Environ(), env...)) //nolint:gosec // this program's own path, re-executed with its own arguments
	if fi, serr := os.Stat(b.Path); serr == nil {
		b.self = fi
	}
	return err
}
