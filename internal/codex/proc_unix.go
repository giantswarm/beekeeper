//go:build unix

package codex

import (
	"os/exec"
	"syscall"
	"time"
)

func configureTurnCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return stopProcessGroup(cmd.Process.Pid)
	}
	cmd.WaitDelay = 2 * time.Second
}

func stopTurnProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = stopProcessGroup(cmd.Process.Pid)
}

func stopProcessGroup(pgid int) error {
	if pgid <= 0 {
		return nil
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); err != nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); err != nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}
