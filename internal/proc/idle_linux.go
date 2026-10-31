//go:build linux

package proc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// StopIdle stops exactly the process identified by pid and started. It freezes
// that process before checking every thread for children and rechecking the
// caller's lifecycle evidence. This closes both PID-reuse and concurrent-fork
// races; an active or unreadable process is resumed and left alone.
func StopIdle(pid int, started time.Time, empty func() bool) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	actual, err := Started(pid)
	if err != nil || started.IsZero() || !actual.Equal(started) {
		return errors.New("process identity changed")
	}
	stopped, _, err := idleThreads(pid)
	if err != nil {
		return err
	}
	if stopped {
		return errors.New("process was already stopped")
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGSTOP, nil, 0); err != nil {
		return err
	}
	defer func() { _ = unix.PidfdSendSignal(fd, unix.SIGCONT, nil, 0) }()
	deadline := time.Now().Add(time.Second)
	for {
		stopped, children, err := idleThreads(pid)
		if err != nil {
			return err
		}
		if stopped {
			if children || !empty() {
				return errors.New("run is active or its evidence changed")
			}
			return unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		}
		if time.Now().After(deadline) {
			return errors.New("process did not stop")
		}
		time.Sleep(time.Millisecond)
	}
}

func idleThreads(pid int) (stopped, children bool, err error) {
	dir := filepath.Join("/proc", strconv.Itoa(pid), "task")
	tasks, err := os.ReadDir(dir)
	if err != nil {
		return false, false, err
	}
	stopped = len(tasks) > 0
	for _, task := range tasks {
		path := filepath.Join(dir, task.Name())
		status, err := os.ReadFile(filepath.Clean(filepath.Join(path, "status")))
		if err != nil {
			return false, false, err
		}
		if !strings.Contains(string(status), "\nState:\tT") {
			stopped = false
		}
		raw, err := os.ReadFile(filepath.Clean(filepath.Join(path, "children")))
		if err != nil {
			return false, false, fmt.Errorf("read children: %w", err)
		}
		children = children || len(strings.Fields(string(raw))) > 0
	}
	return stopped, children, nil
}
