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
// blocks: an inbox no process reads refuses it (ErrNotRunning). Writes are
// serialized by a lock on the inbox, so two senders' lines never
// interleave, whatever their length.
func Send(path, msg string) error {
	line, err := steerLine(msg)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // the agent's inbox under the state folder
	if errors.Is(err, syscall.ENXIO) || errors.Is(err, os.ErrNotExist) {
		return ErrNotRunning
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
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
