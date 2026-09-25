//go:build linux

package cmd

import (
	"os"
	"syscall"
)

// binary is the executable a gate call runs. beekeeper self-update renames a
// new binary over its path; a waiting call notices the other file there and
// re-executes it, so a fix reaches the calls that already wait.
type binary struct {
	path string
	// self is the running file, which /proc/self/exe keeps reaching after
	// the path names another.
	self os.FileInfo
}

// runningBinary is this process's executable, nil when /proc cannot say.
func runningBinary() *binary {
	path, err := os.Executable()
	if err != nil {
		return nil
	}
	self, err := os.Stat("/proc/self/exe")
	if err != nil {
		return nil
	}
	return &binary{path: path, self: self}
}

// replaced says whether the path now names another executable file than
// the running one.
func (b *binary) replaced() bool {
	if b == nil {
		return false
	}
	fi, err := os.Stat(b.path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 && !os.SameFile(fi, b.self)
}

// exec replaces this process with the file at the path: the same pid,
// argument vector, stdio and environment plus env. It returns only when the
// new binary does not start; the call then stays on the running one until
// another file replaces it.
func (b *binary) exec(env ...string) error {
	err := syscall.Exec(b.path, os.Args, append(os.Environ(), env...)) //nolint:gosec // this program's own path, re-executed with its own arguments
	if fi, serr := os.Stat(b.path); serr == nil {
		b.self = fi
	}
	return err
}
