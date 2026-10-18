package proc

import (
	"os"
	"slices"
	"syscall"
	"time"
)

// Terminate sends the processes pids SIGTERM, and SIGKILL to those still
// alive after grace.
func Terminate(pids []int, grace time.Duration) {
	signal := func(sig syscall.Signal) {
		for _, pid := range pids {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Signal(sig)
			}
		}
	}
	signal(syscall.SIGTERM)
	for deadline := time.Now().Add(grace); time.Now().Before(deadline) && slices.ContainsFunc(pids, Alive); {
		time.Sleep(100 * time.Millisecond)
	}
	signal(syscall.SIGKILL)
}
