//go:build darwin

package platform

import (
	"os"
	"syscall"
	"time"
)

// Binary remembers the file this process started from, before an update
// renames another executable over its path.
type Binary struct {
	Path string
	self os.FileInfo
}

var runningBinary = initialBinary()

func initialBinary() *Binary {
	path, err := os.Executable()
	if err != nil {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return &Binary{Path: path, self: fi}
}

// RunningBinary is captured at startup: a later stat alone would remember
// the replacement rather than the executable this process still runs.
func RunningBinary() *Binary { return runningBinary }

// ProcessBinary knows this process; inspecting another process's mapped
// executable is unavailable on this platform.
func ProcessBinary(pid int) *Binary {
	if pid == os.Getpid() {
		return RunningBinary()
	}
	return nil
}

// Replaced reports an executable renamed over the running binary's path.
func (b *Binary) Replaced() bool {
	_, ok := b.Replacement()
	return ok
}

// Replacement is the install renamed over the running binary's path; false
// while the path names the running file, or no executable.
func (b *Binary) Replacement() (Install, bool) {
	if b == nil {
		return Install{}, false
	}
	fi, err := os.Stat(b.Path)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 || os.SameFile(fi, b.self) {
		return Install{}, false
	}
	in := Install{Path: b.Path}
	// A rename sets the change time of the file it moves.
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		in.At = time.Unix(st.Ctimespec.Unix())
	}
	return in, true
}

// Exec keeps the process identity and arguments at the loop's quiet point.
func (b *Binary) Exec(env ...string) error {
	err := syscall.Exec(b.Path, os.Args, append(os.Environ(), env...)) //nolint:gosec // this process's own executable and arguments
	if fi, serr := os.Stat(b.Path); serr == nil {
		b.self = fi
	}
	return err
}
