//go:build linux

package free

import (
	"os"
	"strconv"
	"syscall"
)

// Nice lowers this process's priority to n. Linux keeps a nice value per
// thread, so every thread of the Go runtime is set; threads and children
// started later inherit it.
func Nice(n int) {
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		_ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, n)
		return
	}
	for _, t := range tasks {
		if tid, err := strconv.Atoi(t.Name()); err == nil {
			_ = syscall.Setpriority(syscall.PRIO_PROCESS, tid, n)
		}
	}
}
