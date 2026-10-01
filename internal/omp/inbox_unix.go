//go:build unix

package omp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// MakeInbox creates the inbox FIFO at path, keeping one that exists.
func MakeInbox(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if fi, err := os.Stat(path); err == nil {
		if fi.Mode()&os.ModeNamedPipe == 0 {
			return fmt.Errorf("%s exists and is no FIFO", path)
		}
		return nil
	}
	return syscall.Mkfifo(path, 0o600)
}

// Send writes msg to the inbox at path as one steer command. It never
// blocks on an inbox no process reads: that refuses it (ErrNotRunning).
// Writes are serialized by a lock file beside the inbox (macOS locks no
// FIFO), so two senders' lines never interleave, whatever their length.
func Send(path, msg string) error {
	line, err := steerLine(msg)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return ErrNotRunning
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // beside the agent's inbox under the state folder
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // the agent's inbox under the state folder
	if errors.Is(err, syscall.ENXIO) {
		return ErrNotRunning
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// A full pipe blocks the write until omp reads: the agent is alive.
	if err := setBlocking(f); err != nil {
		return err
	}
	_, err = f.Write(line)
	return err
}

func setBlocking(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = syscall.SetNonblock(int(fd), false) }); err != nil {
		return err
	}
	return serr
}

// RemoveInbox removes the inbox at path and its lock file; removed is
// false when there was none. A process that still reads the inbox keeps
// its open end, but no message reaches it any more.
func RemoveInbox(path string) (removed bool, err error) {
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := os.Remove(path + ".lock"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	return true, nil
}
