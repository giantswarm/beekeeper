//go:build linux

package proc

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func openMatchingProcess(pid int) (processSignals, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return processSignals{}, err
	}
	return processSignals{
		signal: func(sig syscall.Signal) error {
			return unix.PidfdSendSignal(fd, unix.Signal(sig), nil, 0)
		},
		close: func() { _ = unix.Close(fd) },
	}, nil
}
