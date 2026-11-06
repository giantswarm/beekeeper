package proc

import (
	"syscall"

	"golang.org/x/sys/unix"
)

type pidfdHandle int

func openProcessHandle(pid int) (processHandle, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, err
	}
	return pidfdHandle(fd), nil
}

func (h pidfdHandle) signal(sig syscall.Signal) error {
	return unix.PidfdSendSignal(int(h), sig, nil, 0)
}

func (h pidfdHandle) close() error { return unix.Close(int(h)) }
